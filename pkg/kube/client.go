/*
 * Copyright The Kmesh Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"istio.io/istio/pkg/kube/mcs"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	gatewayapiclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	clientextensions "istio.io/client-go/pkg/apis/extensions/v1alpha1"
	clientnetworkingalpha "istio.io/client-go/pkg/apis/networking/v1alpha3"
	clientnetworkingbeta "istio.io/client-go/pkg/apis/networking/v1beta1"
	clientsecurity "istio.io/client-go/pkg/apis/security/v1beta1"
	clienttelemetry "istio.io/client-go/pkg/apis/telemetry/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapi "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayapibeta "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	DefaultLocalAddress      = "localhost"
	DefaultPodRunningTimeout = 30 * time.Second
)

// IstioScheme returns a scheme will all known Istio-related types added
var (
	IstioScheme = istioScheme()
)

func istioScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(kubescheme.AddToScheme(scheme))
	utilruntime.Must(mcs.AddToScheme(scheme))
	utilruntime.Must(clientnetworkingalpha.AddToScheme(scheme))
	utilruntime.Must(clientnetworkingbeta.AddToScheme(scheme))
	utilruntime.Must(clientsecurity.AddToScheme(scheme))
	utilruntime.Must(clienttelemetry.AddToScheme(scheme))
	utilruntime.Must(clientextensions.AddToScheme(scheme))
	utilruntime.Must(gatewayapi.Install(scheme))
	utilruntime.Must(gatewayapibeta.Install(scheme))
	utilruntime.Must(gatewayapiv1.Install(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	return scheme
}

// Client defines the interface for accessing Kubernetes clients and resources.
type Client interface {
	// Kube returns the core kube client
	Kube() kubernetes.Interface

	// GatewayAPI returns the gateway-api kube client.
	GatewayAPI() gatewayapiclient.Interface
}

// CLIClient extends the Client interface with additional functionality for CLI operations.
type CLIClient interface {
	Client

	// PodsForSelector finds pods matching selector.
	PodsForSelector(ctx context.Context, namespace string, labelSelectors ...string) (*v1.PodList, error)

	// UtilFactory returns a kubectl factory
	UtilFactory() PartialFactory

	// NewPortForwarder creates a new PortForwarder configured for the given pod. If localPort=0, a port will be
	// dynamically selected. If localAddress is empty, "localhost" is used.
	NewPortForwarder(podName string, ns string, localAddress string, localPort int, podPort int) (PortForwarder, error)
}

// NewCLIClient creates a new CLIClient using the specified client configuration and optional client options.
func NewCLIClient(clientConfig clientcmd.ClientConfig, opts ...ClientOption) (CLIClient, error) {
	return newClientInternal(newClientFactory(clientConfig, true), opts...)
}

// ClientOption defines an option for configuring the CLIClient.
type ClientOption func(CLIClient) CLIClient

type client struct {
	config *rest.Config

	kube          kubernetes.Interface
	gatewayapi    gatewayapiclient.Interface
	clientFactory *clientFactory
}

func (c *client) NewPortForwarder(podName string, ns string, localAddress string, localPort int, podPort int) (PortForwarder, error) {
	return newPortForwarder(c, podName, ns, localAddress, localPort, podPort)
}

func newPortForwarder(cliClient *client, podName string, ns string, localAddress string, localPort int, podPort int) (PortForwarder, error) {
	ctx, cancel := context.WithCancel(context.Background())

	if localAddress == "" {
		localAddress = DefaultLocalAddress
	}

	localPort, err := getAvailablePort()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to find an available port: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Duration("pod-running-timeout", DefaultPodRunningTimeout, "Timeout for waiting for pod to be running")
	cmd.Flags().String("namespace", KmeshNamespace, "Specify the namespace to use")
	cmd.Flags().StringSlice("address", []string{DefaultLocalAddress}, "Specify the addresses to bind")
	return &portForwarder{
		cmd:              cmd,
		RESTClientGetter: cliClient.UtilFactory(),
		ctx:              ctx,
		cancel:           cancel,
		podName:          podName,
		ns:               ns,
		localAddress:     localAddress,
		localPort:        localPort,
		podPort:          podPort,
		errCh:            make(chan error, 1),
	}, nil
}

func (c *client) UtilFactory() PartialFactory {
	return c.clientFactory
}

// newClientInternal creates a Kubernetes client from the given factory.
func newClientInternal(clientFactory *clientFactory, opts ...ClientOption) (*client, error) {
	var c client
	var err error

	c.clientFactory = clientFactory

	c.config, err = clientFactory.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	for _, opt := range opts {
		opt(&c)
	}

	c.kube, err = kubernetes.NewForConfig(c.config)
	if err != nil {
		return nil, err
	}

	c.gatewayapi, err = gatewayapiclient.NewForConfig(c.config)
	if err != nil {
		return nil, err
	}

	return &c, nil
}

func (c *client) Kube() kubernetes.Interface {
	return c.kube
}

func (c *client) GatewayAPI() gatewayapiclient.Interface {
	return c.gatewayapi
}

func (c *client) PodsForSelector(ctx context.Context, namespace string, labelSelectors ...string) (*v1.PodList, error) {
	return c.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: strings.Join(labelSelectors, ","),
	})
}
