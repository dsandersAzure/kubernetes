/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"fmt"
	"net"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	fedclient "k8s.io/kubernetes/federation/client/clientset_generated/federation_clientset"
	"k8s.io/kubernetes/pkg/api"
	client "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	kubectlcmd "k8s.io/kubernetes/pkg/kubectl/cmd"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	restclient "k8s.io/client-go/rest"
	federationapi "k8s.io/kubernetes/federation/apis/federation"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	// KubeconfigSecretDataKey is the key name used in the secret to
	// stores a cluster's credentials.
	KubeconfigSecretDataKey = "kubeconfig"

	// Used in and to create the kube-dns configmap storing the zone info
	FedDomainMapKey      = "federations"
	KubeDnsConfigmapName = "kube-dns"

	// DefaultFederationSystemNamespace is the namespace in which
	// federation system components are hosted.
	DefaultFederationSystemNamespace = "federation-system"

	// Used to build a clientset for a cluster using the secret
	userAgentName = "kubefed-tool"
	KubeAPIQPS    = 20.0
	KubeAPIBurst  = 30
)

// AdminConfig provides a filesystem based kubeconfig (via
// `PathOptions()`) and a mechanism to talk to the federation
// host cluster and the federation control plane api server.
type AdminConfig interface {
	// PathOptions provides filesystem based kubeconfig access.
	PathOptions() *clientcmd.PathOptions
	// FedClientSet provides a federation API compliant clientset
	// to communicate with the federation control plane api server
	FederationClientset(context, kubeconfigPath string) (*fedclient.Clientset, error)
	// ClusterFactory provides a mechanism to communicate with the
	// cluster derived from the context and the kubeconfig.
	ClusterFactory(context, kubeconfigPath string) cmdutil.Factory
}

// adminConfig implements the AdminConfig interface.
type adminConfig struct {
	pathOptions *clientcmd.PathOptions
}

// NewAdminConfig creates an admin config for `kubefed` commands.
func NewAdminConfig(pathOptions *clientcmd.PathOptions) AdminConfig {
	return &adminConfig{
		pathOptions: pathOptions,
	}
}

func (a *adminConfig) PathOptions() *clientcmd.PathOptions {
	return a.pathOptions
}

func (a *adminConfig) FederationClientset(context, kubeconfigPath string) (*fedclient.Clientset, error) {
	fedConfig := a.getClientConfig(context, kubeconfigPath)
	fedClientConfig, err := fedConfig.ClientConfig()
	if err != nil {
		return nil, err
	}

	return fedclient.NewForConfigOrDie(fedClientConfig), nil
}

func (a *adminConfig) ClusterFactory(context, kubeconfigPath string) cmdutil.Factory {
	hostClientConfig := a.getClientConfig(context, kubeconfigPath)
	return cmdutil.NewFactory(hostClientConfig)
}

func (a *adminConfig) getClientConfig(context, kubeconfigPath string) clientcmd.ClientConfig {
	loadingRules := *a.pathOptions.LoadingRules
	loadingRules.Precedence = a.pathOptions.GetLoadingPrecedence()
	loadingRules.ExplicitPath = kubeconfigPath
	overrides := &clientcmd.ConfigOverrides{
		CurrentContext: context,
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&loadingRules, overrides)
}

// SubcommandOptions holds the configuration required by the subcommands of
// `kubefed`.
type SubcommandOptions struct {
	Name                      string
	Host                      string
	FederationSystemNamespace string
	Kubeconfig                string
}

func (o *SubcommandOptions) Bind(flags *pflag.FlagSet) {
	flags.StringVar(&o.Kubeconfig, "kubeconfig", "", "Path to the kubeconfig file to use for CLI requests.")
	flags.StringVar(&o.Host, "host-cluster-context", "", "Host cluster context")
	flags.StringVar(&o.FederationSystemNamespace, "federation-system-namespace", DefaultFederationSystemNamespace, "Namespace in the host cluster where the federation system components are installed")
}

func (o *SubcommandOptions) SetName(cmd *cobra.Command, args []string) error {
	name, err := kubectlcmd.NameFromCommandArgs(cmd, args)
	if err != nil {
		return err
	}
	o.Name = name
	return nil
}

func CreateKubeconfigSecret(clientset *client.Clientset, kubeconfig *clientcmdapi.Config, namespace, name string, dryRun bool) (*api.Secret, error) {
	configBytes, err := clientcmd.Write(*kubeconfig)
	if err != nil {
		return nil, err
	}

	// Build the secret object with the minified and flattened
	// kubeconfig content.
	secret := &api.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			KubeconfigSecretDataKey: configBytes,
		},
	}

	if !dryRun {
		return clientset.Core().Secrets(namespace).Create(secret)
	}
	return secret, nil
}

var kubeconfigGetterForSecret = func(secret *api.Secret) clientcmd.KubeconfigGetter {
	return func() (*clientcmdapi.Config, error) {
		var data []byte
		ok := false
		data, ok = secret.Data[KubeconfigSecretDataKey]
		if !ok {
			return nil, fmt.Errorf("secret does not have data with key: %s", KubeconfigSecretDataKey)
		}
		return clientcmd.Load(data)
	}
}

func GetClientsetFromSecret(secret *api.Secret, serverAddress string) (*client.Clientset, error) {
	clusterConfig, err := buildConfigFromSecret(secret, serverAddress)
	if err == nil && clusterConfig != nil {
		clientset := client.NewForConfigOrDie(restclient.AddUserAgent(clusterConfig, userAgentName))
		return clientset, nil
	}
	return nil, err
}

func GetServerAddress(c *federationapi.Cluster) (string, error) {
	hostIP, err := utilnet.ChooseHostInterface()
	if err != nil {
		return "", err
	}

	for _, item := range c.Spec.ServerAddressByClientCIDRs {
		_, cidrnet, err := net.ParseCIDR(item.ClientCIDR)
		if err != nil {
			return "", err
		}
		if cidrnet.Contains(hostIP) {
			return item.ServerAddress, nil
		}
	}

	return "", nil
}

func buildConfigFromSecret(secret *api.Secret, serverAddress string) (*restclient.Config, error) {
	kubeconfigGetter := kubeconfigGetterForSecret(secret)
	clusterConfig, err := clientcmd.BuildConfigFromKubeconfigGetter(serverAddress, kubeconfigGetter)
	if err != nil {
		return nil, err
	}
	clusterConfig.QPS = KubeAPIQPS
	clusterConfig.Burst = KubeAPIBurst

	return clusterConfig, nil
}
