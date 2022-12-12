package argo

import (
	"bytes"
	"context"
	"fmt"
	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/logging"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/mitchellh/go-homedir"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	apimachineryschema "k8s.io/apimachinery/pkg/runtime/schema"
	restclient "k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	aggregator "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
)

const defaultFieldManagerName = "Terraform"

func Provider() *schema.Provider {
	p := &schema.Provider{
		Schema: map[string]*schema.Schema{
			"host": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_HOST", ""),
				Description: "The hostname (in form of URI) of Kubernetes master.",
			},
			"username": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_USER", ""),
				Description: "The username to use for HTTP basic authentication when accessing the Kubernetes master endpoint.",
			},
			"password": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_PASSWORD", ""),
				Description: "The password to use for HTTP basic authentication when accessing the Kubernetes master endpoint.",
			},
			"insecure": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_INSECURE", false),
				Description: "Whether server should be accessed without verifying the TLS certificate.",
			},
			"client_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CLIENT_CERT_DATA", ""),
				Description: "PEM-encoded client certificate for TLS authentication.",
			},
			"client_key": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CLIENT_KEY_DATA", ""),
				Description: "PEM-encoded client certificate key for TLS authentication.",
			},
			"cluster_ca_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CLUSTER_CA_CERT_DATA", ""),
				Description: "PEM-encoded root certificates bundle for TLS authentication.",
			},
			"config_paths": {
				Type:        schema.TypeList,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Optional:    true,
				Description: "A list of paths to kube config files. Can be set with KUBE_CONFIG_PATHS environment variable.",
			},
			"config_path": {
				Type:          schema.TypeString,
				Optional:      true,
				DefaultFunc:   schema.EnvDefaultFunc("KUBE_CONFIG_PATH", nil),
				Description:   "Path to the kube config file. Can be set with KUBE_CONFIG_PATH.",
				ConflictsWith: []string{"config_paths"},
			},
			"config_context": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CTX", ""),
			},
			"config_context_auth_info": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CTX_AUTH_INFO", ""),
				Description: "",
			},
			"config_context_cluster": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CTX_CLUSTER", ""),
				Description: "",
			},
			"token": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_TOKEN", ""),
				Description: "Token to authenticate an service account",
			},
			"proxy_url": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "URL to the proxy to be used for all API requests",
				DefaultFunc: schema.EnvDefaultFunc("KUBE_PROXY_URL", ""),
			},
			"exec": {
				Type:     schema.TypeList,
				Optional: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"api_version": {
							Type:     schema.TypeString,
							Required: true,
							ValidateDiagFunc: func(val interface{}, key cty.Path) diag.Diagnostics {
								apiVersion := val.(string)
								if apiVersion == "client.authentication.k8s.io/v1alpha1" {
									return diag.Diagnostics{{
										Severity: diag.Warning,
										Summary:  "v1alpha1 of the client authentication API is deprecated, use v1beta1 or above",
										Detail:   "v1alpha1 of the client authentication API will be removed in Kubernetes client versions 1.24 and above. You may need to update your exec plugin to use the latest version.",
									}}
								}
								return nil
							},
						},
						"command": {
							Type:     schema.TypeString,
							Required: true,
						},
						"env": {
							Type:     schema.TypeMap,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
						"args": {
							Type:     schema.TypeList,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
					},
				},
				Description: "",
			},
			"experiments": {
				Type:        schema.TypeList,
				MaxItems:    1,
				Optional:    true,
				Description: "Enable and disable experimental features.",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"manifest_resource": {
							Type:     schema.TypeBool,
							Optional: true,
							DefaultFunc: func() (interface{}, error) {
								if v := os.Getenv("TF_X_KUBERNETES_MANIFEST_RESOURCE"); v != "" {
									vv, err := strconv.ParseBool(v)
									if err != nil {
										return true, err
									}
									return vv, nil
								}
								return true, nil
							},
							Description: "Enable the `kubernetes_manifest` resource.",
						},
					},
				},
			},
			"ignore_annotations": {
				Type: schema.TypeList,
				Elem: &schema.Schema{
					Type:         schema.TypeString,
					ValidateFunc: validation.StringIsValidRegExp,
				},
				Optional:    true,
				Description: "List of Kubernetes metadata annotations to ignore across all resources handled by this provider for situations where external systems are managing certain resource annotations. Each item is a regular expression.",
			},
			"ignore_labels": {
				Type: schema.TypeList,
				Elem: &schema.Schema{
					Type:         schema.TypeString,
					ValidateFunc: validation.StringIsValidRegExp,
				},
				Optional:    true,
				Description: "List of Kubernetes metadata labels to ignore across all resources handled by this provider for situations where external systems are managing certain resource labels. Each item is a regular expression.",
			},
		},

		DataSourcesMap: map[string]*schema.Resource{},

		ResourcesMap: map[string]*schema.Resource{},
	}

	p.ConfigureContextFunc = func(ctx context.Context, d *schema.ResourceData) (interface{}, diag.Diagnostics) {
		return providerConfigure(ctx, d, p.TerraformVersion)
	}

	return p
}

type kubeClientsets struct {
	// TODO: this struct has become overloaded we should
	// rename this or break it into smaller structs
	config              *restclient.Config
	mainClientset       *kubernetes.Clientset
	aggregatorClientset *aggregator.Clientset
	dynamicClient       dynamic.Interface
	discoveryClient     discovery.DiscoveryInterface

	IgnoreAnnotations []string
	IgnoreLabels      []string
}

func providerConfigure(ctx context.Context, d *schema.ResourceData, terraformVersion string) (interface{}, diag.Diagnostics) {
	// Config initialization
	cfg, err := initializeConfiguration(d)
	if err != nil {
		return nil, diag.FromErr(err)
	}
	if cfg == nil {
		// This is a TEMPORARY measure to work around https://github.com/hashicorp/terraform/issues/24055
		// IMPORTANT: this will NOT enable a workaround of issue: https://github.com/hashicorp/terraform/issues/4149
		// IMPORTANT: if the supplied configuration is incomplete or invalid
		///IMPORTANT: provider operations will fail or attempt to connect to localhost endpoints
		cfg = &restclient.Config{}
	}

	cfg.UserAgent = fmt.Sprintf("HashiCorp/1.0 Terraform/%s", terraformVersion)

	if logging.IsDebugOrHigher() {
		log.Printf("[DEBUG] Enabling HTTP requests/responses tracing")
		cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
			return logging.NewTransport("Kubernetes", rt)
		}
	}

	ignoreAnnotations := []string{}
	ignoreLabels := []string{}

	if v, ok := d.Get("ignore_annotations").([]interface{}); ok {
		ignoreAnnotations = expandStringSlice(v)
	}
	if v, ok := d.Get("ignore_labels").([]interface{}); ok {
		ignoreLabels = expandStringSlice(v)
	}

	m := kubeClientsets{
		config:              cfg,
		mainClientset:       nil,
		aggregatorClientset: nil,
		IgnoreAnnotations:   ignoreAnnotations,
		IgnoreLabels:        ignoreLabels,
	}
	return m, diag.Diagnostics{}
}

func initializeConfiguration(d *schema.ResourceData) (*restclient.Config, error) {
	overrides := &clientcmd.ConfigOverrides{}
	loader := &clientcmd.ClientConfigLoadingRules{}

	configPaths := []string{}

	if v, ok := d.Get("config_path").(string); ok && v != "" {
		configPaths = []string{v}
	} else if v, ok := d.Get("config_paths").([]interface{}); ok && len(v) > 0 {
		for _, p := range v {
			configPaths = append(configPaths, p.(string))
		}
	} else if v := os.Getenv("KUBE_CONFIG_PATHS"); v != "" {
		// NOTE we have to do this here because the schema
		// does not yet allow you to set a default for a TypeList
		configPaths = filepath.SplitList(v)
	}

	if len(configPaths) > 0 {
		expandedPaths := []string{}
		for _, p := range configPaths {
			path, err := homedir.Expand(p)
			if err != nil {
				return nil, err
			}

			log.Printf("[DEBUG] Using kubeconfig: %s", path)
			expandedPaths = append(expandedPaths, path)
		}

		if len(expandedPaths) == 1 {
			loader.ExplicitPath = expandedPaths[0]
		} else {
			loader.Precedence = expandedPaths
		}

		ctxSuffix := "; default context"

		kubectx, ctxOk := d.GetOk("config_context")
		authInfo, authInfoOk := d.GetOk("config_context_auth_info")
		cluster, clusterOk := d.GetOk("config_context_cluster")
		if ctxOk || authInfoOk || clusterOk {
			ctxSuffix = "; overriden context"
			if ctxOk {
				overrides.CurrentContext = kubectx.(string)
				ctxSuffix += fmt.Sprintf("; config ctx: %s", overrides.CurrentContext)
				log.Printf("[DEBUG] Using custom current context: %q", overrides.CurrentContext)
			}

			overrides.Context = clientcmdapi.Context{}
			if authInfoOk {
				overrides.Context.AuthInfo = authInfo.(string)
				ctxSuffix += fmt.Sprintf("; auth_info: %s", overrides.Context.AuthInfo)
			}
			if clusterOk {
				overrides.Context.Cluster = cluster.(string)
				ctxSuffix += fmt.Sprintf("; cluster: %s", overrides.Context.Cluster)
			}
			log.Printf("[DEBUG] Using overidden context: %#v", overrides.Context)
		}
	}

	// Overriding with static configuration
	if v, ok := d.GetOk("insecure"); ok {
		overrides.ClusterInfo.InsecureSkipTLSVerify = v.(bool)
	}
	if v, ok := d.GetOk("cluster_ca_certificate"); ok {
		overrides.ClusterInfo.CertificateAuthorityData = bytes.NewBufferString(v.(string)).Bytes()
	}
	if v, ok := d.GetOk("client_certificate"); ok {
		overrides.AuthInfo.ClientCertificateData = bytes.NewBufferString(v.(string)).Bytes()
	}
	if v, ok := d.GetOk("host"); ok {
		// Server has to be the complete address of the kubernetes cluster (scheme://hostname:port), not just the hostname,
		// because `overrides` are processed too late to be taken into account by `defaultServerUrlFor()`.
		// This basically replicates what defaultServerUrlFor() does with config but for overrides,
		// see https://github.com/kubernetes/client-go/blob/v12.0.0/rest/url_utils.go#L85-L87
		hasCA := len(overrides.ClusterInfo.CertificateAuthorityData) != 0
		hasCert := len(overrides.AuthInfo.ClientCertificateData) != 0
		defaultTLS := hasCA || hasCert || overrides.ClusterInfo.InsecureSkipTLSVerify
		host, _, err := restclient.DefaultServerURL(v.(string), "", apimachineryschema.GroupVersion{}, defaultTLS)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse host: %s", err)
		}

		overrides.ClusterInfo.Server = host.String()
	}
	if v, ok := d.GetOk("username"); ok {
		overrides.AuthInfo.Username = v.(string)
	}
	if v, ok := d.GetOk("password"); ok {
		overrides.AuthInfo.Password = v.(string)
	}
	if v, ok := d.GetOk("client_key"); ok {
		overrides.AuthInfo.ClientKeyData = bytes.NewBufferString(v.(string)).Bytes()
	}
	if v, ok := d.GetOk("token"); ok {
		overrides.AuthInfo.Token = v.(string)
	}

	if v, ok := d.GetOk("exec"); ok {
		exec := &clientcmdapi.ExecConfig{}
		if spec, ok := v.([]interface{})[0].(map[string]interface{}); ok {
			exec.InteractiveMode = clientcmdapi.IfAvailableExecInteractiveMode
			exec.APIVersion = spec["api_version"].(string)
			exec.Command = spec["command"].(string)
			exec.Args = expandStringSlice(spec["args"].([]interface{}))
			for kk, vv := range spec["env"].(map[string]interface{}) {
				exec.Env = append(exec.Env, clientcmdapi.ExecEnvVar{Name: kk, Value: vv.(string)})
			}
		} else {
			return nil, fmt.Errorf("Failed to parse exec")
		}
		overrides.AuthInfo.Exec = exec
	}

	if v, ok := d.GetOk("proxy_url"); ok {
		overrides.ClusterDefaults.ProxyURL = v.(string)
	}

	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		log.Printf("[WARN] Invalid provider configuration was supplied. Provider operations likely to fail: %v", err)
		return nil, nil
	}

	return cfg, nil
}

func expandStringSlice(s []interface{}) []string {
	result := make([]string, len(s), len(s))
	for k, v := range s {
		// Handle the Terraform parser bug which turns empty strings in lists to nil.
		if v == nil {
			result[k] = ""
		} else {
			result[k] = v.(string)
		}
	}
	return result
}
