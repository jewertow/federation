// Copyright Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the License);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an AS IS BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/spf13/cobra"
	istiokube "istio.io/istio/pkg/kube"
	istiolog "istio.io/istio/pkg/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/env"

	"github.com/openshift-service-mesh/federation/internal/pkg/config"
	"github.com/openshift-service-mesh/federation/internal/pkg/fds"
	"github.com/openshift-service-mesh/federation/internal/pkg/informer"
	"github.com/openshift-service-mesh/federation/internal/pkg/istio"
	"github.com/openshift-service-mesh/federation/internal/pkg/kube"
	"github.com/openshift-service-mesh/federation/internal/pkg/mcp"
	"github.com/openshift-service-mesh/federation/internal/pkg/xds"
	"github.com/openshift-service-mesh/federation/internal/pkg/xds/adsc"
	"github.com/openshift-service-mesh/federation/internal/pkg/xds/adss"
)

var (
	// Global variables to store the parsed commandline arguments
	meshPeers, exportedServiceSet, importedServiceSet, configMode string

	loggingOptions = istiolog.DefaultOptions()
	log            = istiolog.RegisterScope("default", "default logging scope")
)

// NewRootCommand returns the root cobra command of federation-controller
func NewRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:          "federation-controller",
		Short:        "Istio Federation.",
		Long:         "Federation is a controller that utilizes MCP protocol to configure mesh-federation in Istio.",
		SilenceUsage: true,
		PreRunE: func(c *cobra.Command, args []string) error {
			c.PersistentFlags().AddGoFlagSet(flag.CommandLine)
			return nil
		},
	}
	addFlags(rootCmd)
	return rootCmd
}

func addFlags(c *cobra.Command) {
	// Process commandline args.
	c.PersistentFlags().StringVar(&meshPeers, "meshPeers", "",
		"Mesh peers that include address ip/hostname to remote Peer, and the ports for dataplane and discovery")
	c.PersistentFlags().StringVar(&exportedServiceSet, "exportedServiceSet", "",
		"ExportedServiceSet that include selectors to match the services that will be exported")
	c.PersistentFlags().StringVar(&importedServiceSet, "importedServiceSet", "",
		"ImportedServiceSet that include selectors to match the services that will be imported")
	c.PersistentFlags().StringVar(&configMode, "configMode", "",
		"Specifies how the federation controller provides mesh config to istiod. Valid options include \"mcp\" and \"k8s\".")

	// Attach the Istio logging options to the command.
	loggingOptions.AttachCobraFlags(c)
}

// unmarshalJSON is a utility function to unmarshal a YAML string into a struct
// and return an error if the unmarshalling fails.
func unmarshalJSON(input string, out interface{}) error {
	if err := json.Unmarshal([]byte(input), out); err != nil {
		return fmt.Errorf("failed to unmarshal JSON: %w", err)
	}
	return nil
}

// Parse the command line arguments by using the flag package
// Export the parsed arguments to the AppConfig variable
func parse() (*config.Federation, error) {
	var (
		peers    config.MeshPeers
		exported config.ExportedServiceSet
		imported config.ImportedServiceSet
	)

	if err := unmarshalJSON(meshPeers, &peers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal mesh peers: %w", err)
	}
	if err := unmarshalJSON(exportedServiceSet, &exported); err != nil {
		return nil, fmt.Errorf("failed to unmarshal exported services: %w", err)
	}
	if importedServiceSet != "" {
		if err := unmarshalJSON(importedServiceSet, &imported); err != nil {
			return nil, fmt.Errorf("failed to unmarshal imported services: %w", err)
		}
	}
	cfgMode := config.ConfigMode(configMode)
	if cfgMode != config.ConfigModeMCP && cfgMode != config.ConfigModeK8s {
		return nil, fmt.Errorf("invalid ConfigMode: %s", cfgMode)
	}

	return &config.Federation{
		MeshPeers:          peers,
		ExportedServiceSet: exported,
		ImportedServiceSet: imported,
		ConfigMode:         cfgMode,
	}, nil
}

func main() {
	rootCmd := NewRootCommand()
	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("failed to execute root command: %v", err)
	}

	cfg, err := parse()
	if err != nil {
		log.Fatalf("failed to parse configuration passed to the program arguments: %v", err)
	}
	log.Infof("Configuration: %v", cfg)

	if err := istiolog.Configure(loggingOptions); err != nil {
		log.Fatalf("failed to configure logging options: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("failed to create in-cluster config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatalf("failed to create Kubernetes clientset: %v", err.Error())
	}

	fdsPushRequests := make(chan xds.PushRequest)
	meshConfigPushRequests := make(chan xds.PushRequest)

	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	serviceInformer := informerFactory.Core().V1().Services().Informer()
	serviceLister := informerFactory.Core().V1().Services().Lister()
	informerFactory.Start(ctx.Done())

	serviceController, err := informer.NewResourceController(serviceInformer, corev1.Service{},
		informer.NewServiceExportEventHandler(*cfg, fdsPushRequests, meshConfigPushRequests))
	if err != nil {
		log.Fatalf("failed to create service informer: %v", err)
	}
	serviceController.RunAndWait(ctx.Done())

	importedServiceStore := fds.NewImportedServiceStore()

	var controllerServiceFQDN string
	if controllerServiceFQDN = env.GetString("CONTROLLER_SERVICE_FQDN", ""); controllerServiceFQDN == "" {
		log.Fatalf("did not find environment variable CONTROLLER_SERVICE_FQDN")
	}
	istioConfigFactory := istio.NewConfigFactory(*cfg, serviceLister, importedServiceStore, controllerServiceFQDN)

	triggerFDSPushOnNewSubscription := func() {
		fdsPushRequests <- xds.PushRequest{
			TypeUrl: xds.ExportedServiceTypeUrl,
		}
	}
	federationServer := adss.NewServer(
		&adss.ServerOpts{Port: 15080, ServerID: "fds"},
		fdsPushRequests,
		triggerFDSPushOnNewSubscription,
		fds.NewExportedServicesGenerator(*cfg, serviceLister),
	)
	go func() {
		if err := federationServer.Run(ctx); err != nil {
			log.Fatalf("failed to start FDS server: %v", err)
		}
	}()

	var startFDSClient func()
	if len(cfg.MeshPeers.Remote.Addresses) > 0 {
		federationClient, err := adsc.New(&adsc.ADSCConfig{
			DiscoveryAddr: fmt.Sprintf("remote-federation-controller.%s.svc.cluster.local:15080", cfg.MeshPeers.Local.ControlPlane.Namespace),
			InitialDiscoveryRequests: []*discovery.DiscoveryRequest{{
				TypeUrl: xds.ExportedServiceTypeUrl,
			}},
			Handlers: map[string]adsc.ResponseHandler{
				xds.ExportedServiceTypeUrl: fds.NewImportedServiceHandler(importedServiceStore, meshConfigPushRequests),
			},
		})
		if err != nil {
			log.Fatalf("failed to creates FDS client: %v", err)
		}
		startFDSClient = func() {
			go func() {
				if err := federationClient.Run(); err != nil {
					log.Fatalf("failed to start FDS client: %v", err)
				}
			}()
		}
	}

	if cfg.ConfigMode == config.ConfigModeMCP {
		mcpServer := adss.NewServer(
			&adss.ServerOpts{Port: 15010, ServerID: "mcp"},
			meshConfigPushRequests,
			startFDSClient,
			mcp.NewGatewayResourceGenerator(istioConfigFactory),
			mcp.NewServiceEntryGenerator(istioConfigFactory),
			mcp.NewWorkloadEntryGenerator(istioConfigFactory),
			mcp.NewVirtualServiceResourceGenerator(istioConfigFactory),
			mcp.NewDestinationRuleResourceGenerator(istioConfigFactory),
		)
		if err := mcpServer.Run(ctx); err != nil {
			log.Fatalf("Error running XDS server: %v", err)
		}
	} else if cfg.ConfigMode == config.ConfigModeK8s {
		istioClient, err := istiokube.NewClient(istiokube.NewClientConfigForRestConfig(kubeConfig), "")
		if err != nil {
			log.Fatalf("failed to create Istio client: %v", err)
		}

		rm := kube.NewReconcilerManager(
			meshConfigPushRequests,
			kube.NewGatewayResourceReconciler(istioClient, istioConfigFactory),
			kube.NewServiceEntryReconciler(istioClient, istioConfigFactory),
			kube.NewWorkloadEntryReconciler(istioClient, istioConfigFactory),
			kube.NewVirtualServiceReconciler(istioClient, istioConfigFactory),
			kube.NewDestinationRuleReconciler(istioClient, istioConfigFactory),
		)
		if err := rm.ReconcileAll(ctx); err != nil {
			log.Fatalf("initial Istio resource reconciliation failed: %v", err)
		}
		go rm.Start(ctx)

		// TODO: implement an appropriate wait allowing proxy to receive all initial
		// federation config before starting FDS client.
		time.Sleep(time.Second * 5)

		if startFDSClient != nil {
			log.Info("Starting FDS client")
			startFDSClient()
		}

		select {}
	}

	os.Exit(0)
}
