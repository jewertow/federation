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

package istio

import (
	"fmt"
	"sort"

	istionetv1alpha3 "istio.io/api/networking/v1alpha3"
	"istio.io/client-go/pkg/apis/networking/v1alpha3"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/listers/core/v1"

	"github.com/openshift-service-mesh/federation/internal/api/federation/v1alpha1"
	"github.com/openshift-service-mesh/federation/internal/pkg/config"
	"github.com/openshift-service-mesh/federation/internal/pkg/fds"
)

const (
	federationIngressGatewayName = "federation-ingress-gateway"
)

type ConfigFactory struct {
	cfg                   config.Federation
	serviceLister         v1.ServiceLister
	importedServiceStore  *fds.ImportedServiceStore
	controllerServiceFQDN string
}

func NewConfigFactory(cfg config.Federation, serviceLister v1.ServiceLister, importedServiceStore *fds.ImportedServiceStore, controllerServiceFQDN string) *ConfigFactory {
	return &ConfigFactory{
		cfg:                   cfg,
		serviceLister:         serviceLister,
		importedServiceStore:  importedServiceStore,
		controllerServiceFQDN: controllerServiceFQDN,
	}
}

func (cf *ConfigFactory) GetDestinationRules() *v1alpha3.DestinationRule {
	if len(cf.cfg.MeshPeers.Remote.Addresses) == 0 {
		return nil
	}
	return &v1alpha3.DestinationRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "originate-istio-mtls-to-remote-federation-controller",
			Namespace: cf.cfg.MeshPeers.Local.ControlPlane.Namespace,
		},
		Spec: istionetv1alpha3.DestinationRule{
			Host: fmt.Sprintf("remote-federation-controller.%s.svc.cluster.local", cf.cfg.MeshPeers.Local.ControlPlane.Namespace),
			TrafficPolicy: &istionetv1alpha3.TrafficPolicy{
				Tls: &istionetv1alpha3.ClientTLSSettings{
					Mode: istionetv1alpha3.ClientTLSSettings_ISTIO_MUTUAL,
					// SNI must come from the configured identity
					Sni: fmt.Sprintf("federation-controller.%s.svc.cluster.local", cf.cfg.MeshPeers.Local.ControlPlane.Namespace),
				},
			},
		},
	}
}

func (cf *ConfigFactory) GetIngressGateway() (*v1alpha3.Gateway, error) {
	gateway := &v1alpha3.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      federationIngressGatewayName,
			Namespace: cf.cfg.MeshPeers.Local.ControlPlane.Namespace,
		},
		Spec: istionetv1alpha3.Gateway{
			Selector: cf.cfg.MeshPeers.Local.Gateways.Ingress.Selector,
			Servers: []*istionetv1alpha3.Server{{
				Hosts: []string{"*"},
				Port: &istionetv1alpha3.Port{
					Number:   cf.cfg.MeshPeers.Local.Gateways.Ingress.Ports.GetDiscoveryPort(),
					Name:     "discovery",
					Protocol: "TLS",
				},
				Tls: &istionetv1alpha3.ServerTLSSettings{
					Mode: istionetv1alpha3.ServerTLSSettings_ISTIO_MUTUAL,
				},
			}},
		},
	}

	var hosts []string
	for _, exportLabelSelector := range cf.cfg.ExportedServiceSet.GetLabelSelectors() {
		matchLabels := labels.SelectorFromSet(exportLabelSelector.MatchLabels)
		services, err := cf.serviceLister.List(matchLabels)
		if err != nil {
			return nil, fmt.Errorf("error listing services (selector=%s): %v", matchLabels, err)
		}
		for _, svc := range services {
			hosts = append(hosts, fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace))
		}
	}
	if len(hosts) == 0 {
		return gateway, nil
	}
	// ServiceLister.List is not idempotent, so to avoid redundant XDS push from Istio to proxies,
	// we must return hostnames in the same order.
	sort.Strings(hosts)

	gateway.Spec.Servers = append(gateway.Spec.Servers, &istionetv1alpha3.Server{
		Hosts: hosts,
		Port: &istionetv1alpha3.Port{
			Number:   cf.cfg.MeshPeers.Local.Gateways.Ingress.Ports.GetDataPlanePort(),
			Name:     "data-plane",
			Protocol: "TLS",
		},
		Tls: &istionetv1alpha3.ServerTLSSettings{
			Mode: istionetv1alpha3.ServerTLSSettings_AUTO_PASSTHROUGH,
		},
	})
	return gateway, nil
}

func (cf *ConfigFactory) GetServiceEntries() ([]*v1alpha3.ServiceEntry, error) {
	var serviceEntries []*v1alpha3.ServiceEntry
	for _, importedSvc := range cf.importedServiceStore.GetAll() {
		// enable Istio mTLS
		importedSvc.Labels["security.istio.io/tlsMode"] = "istio"

		_, err := cf.serviceLister.Services(importedSvc.Namespace).Get(importedSvc.Name)
		if err != nil {
			if !errors.IsNotFound(err) {
				return nil, fmt.Errorf("failed to get Service %s/%s: %v", importedSvc.Name, importedSvc.Namespace, err)
			}
			// Service doesn't exist - create ServiceEntry.
			var ports []*istionetv1alpha3.ServicePort
			for _, port := range importedSvc.Ports {
				ports = append(ports, &istionetv1alpha3.ServicePort{
					Name:       port.Name,
					Number:     port.Number,
					Protocol:   port.Protocol,
					TargetPort: port.TargetPort,
				})
			}
			serviceEntries = append(serviceEntries, &v1alpha3.ServiceEntry{
				ObjectMeta: metav1.ObjectMeta{
					// TODO: add peer name to ensure uniqueness when more than 2 peers are connected
					Name:      fmt.Sprintf("import-%s-%s", importedSvc.Name, importedSvc.Namespace),
					Namespace: cf.cfg.MeshPeers.Local.ControlPlane.Namespace,
					Labels:    map[string]string{"federation.istio-ecosystem.io/peer": "todo"},
				},
				Spec: istionetv1alpha3.ServiceEntry{
					Hosts:      []string{fmt.Sprintf("%s.%s.svc.cluster.local", importedSvc.Name, importedSvc.Namespace)},
					Ports:      ports,
					Endpoints:  cf.makeWorkloadEntrySpecs(importedSvc.Ports, importedSvc.Labels),
					Location:   istionetv1alpha3.ServiceEntry_MESH_INTERNAL,
					Resolution: istionetv1alpha3.ServiceEntry_STATIC,
				},
			})
		}
	}
	if se := cf.getServiceEntryForRemoteFederationController(); se != nil {
		serviceEntries = append(serviceEntries, se)
	}
	return serviceEntries, nil
}

func (cf *ConfigFactory) GetWorkloadEntries() ([]*v1alpha3.WorkloadEntry, error) {
	var workloadEntries []*v1alpha3.WorkloadEntry
	for _, importedSvc := range cf.importedServiceStore.GetAll() {
		// enable Istio mTLS
		importedSvc.Labels["security.istio.io/tlsMode"] = "istio"

		_, err := cf.serviceLister.Services(importedSvc.Namespace).Get(importedSvc.Name)
		if err != nil {
			if !errors.IsNotFound(err) {
				return nil, fmt.Errorf("failed to get Service %s/%s: %v", importedSvc.Name, importedSvc.Namespace, err)
			}
		} else {
			// Service already exists - create WorkloadEntries.
			workloadEntrySpecs := cf.makeWorkloadEntrySpecs(importedSvc.Ports, importedSvc.Labels)
			for idx, weSpec := range workloadEntrySpecs {
				workloadEntries = append(workloadEntries, &v1alpha3.WorkloadEntry{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("import-%s-%d", importedSvc.Name, idx),
						Namespace: importedSvc.Namespace,
						Labels:    map[string]string{"federation.istio-ecosystem.io/peer": "todo"},
					},
					Spec: *weSpec.DeepCopy(),
				})
			}
		}
	}
	return workloadEntries, nil
}

func (cf *ConfigFactory) GetVirtualServices() *v1alpha3.VirtualService {
	return &v1alpha3.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      federationIngressGatewayName,
			Namespace: cf.cfg.MeshPeers.Local.ControlPlane.Namespace,
		},
		Spec: istionetv1alpha3.VirtualService{
			Hosts:    []string{"*"},
			Gateways: []string{federationIngressGatewayName},
			Tcp: []*istionetv1alpha3.TCPRoute{{
				Match: []*istionetv1alpha3.L4MatchAttributes{{
					Port: cf.cfg.MeshPeers.Local.Gateways.Ingress.Ports.GetDiscoveryPort(),
				}},
				Route: []*istionetv1alpha3.RouteDestination{{
					Destination: &istionetv1alpha3.Destination{
						Host: cf.controllerServiceFQDN,
						Port: &istionetv1alpha3.PortSelector{
							Number: 15080,
						},
					},
				}},
			}},
		},
	}
}

func (cf *ConfigFactory) getServiceEntryForRemoteFederationController() *v1alpha3.ServiceEntry {
	if len(cf.cfg.MeshPeers.Remote.Addresses) == 0 {
		return nil
	}

	var endpoints []*istionetv1alpha3.WorkloadEntry
	for _, remoteAddr := range cf.cfg.MeshPeers.Remote.Addresses {
		endpoints = append(endpoints, &istionetv1alpha3.WorkloadEntry{
			Address: remoteAddr,
			Labels:  map[string]string{"security.istio.io/tlsMode": "istio"},
		})
	}
	return &v1alpha3.ServiceEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "remote-federation-controller",
			Namespace: cf.cfg.MeshPeers.Local.ControlPlane.Namespace,
			Labels: map[string]string{
				"istio.io/use-waypoint":              "waypoint",
				"federation.istio-ecosystem.io/peer": "todo",
			},
		},
		Spec: istionetv1alpha3.ServiceEntry{
			Hosts: []string{fmt.Sprintf("remote-federation-controller.%s.svc.cluster.local", cf.cfg.MeshPeers.Local.ControlPlane.Namespace)},
			Ports: []*istionetv1alpha3.ServicePort{{
				Name:     "discovery",
				Number:   15080,
				Protocol: "GRPC",
			}},
			//Location: istionetv1alpha3.ServiceEntry_MESH_EXTERNAL,
			// In case of IP addresses, we set STATIC resolution type to let Envoy
			Resolution: istionetv1alpha3.ServiceEntry_DNS,
			Endpoints:  endpoints,
		},
	}
}

func (cf *ConfigFactory) makeWorkloadEntrySpecs(ports []*v1alpha1.ServicePort, labels map[string]string) []*istionetv1alpha3.WorkloadEntry {
	var workloadEntries []*istionetv1alpha3.WorkloadEntry
	for _, addr := range cf.cfg.MeshPeers.Remote.Addresses {
		we := &istionetv1alpha3.WorkloadEntry{
			Address: addr,
			Network: cf.cfg.MeshPeers.Remote.Network,
			Labels:  labels,
			Ports:   make(map[string]uint32, len(ports)),
		}
		for _, p := range ports {
			we.Ports[p.Name] = cf.cfg.MeshPeers.Remote.Ports.GetDataPlanePort()
		}
		workloadEntries = append(workloadEntries, we)
	}
	return workloadEntries
}
