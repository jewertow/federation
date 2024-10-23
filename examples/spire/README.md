### Install Cert Manager

```shell
helm repo add jetstack https://charts.jetstack.io --force-update
helm-east install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --version v1.16.1 \
  --set crds.enabled=true
helm-west install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --version v1.16.1 \
  --set crds.enabled=true
```

### Create cluster issuer

```shell
keast create secret tls ca-key-cert --cert=east/root-cert.pem --key=east/root-key.pem -n cert-manager
kwest create secret tls ca-key-cert --cert=west/root-cert.pem --key=west/root-key.pem -n cert-manager
keast apply -f examples/spire/cluster-issuer.yaml
kwest apply -f examples/spire/cluster-issuer.yaml
```

### Install SPIRE

1. Deploy SPIRE server and agent:
```shell
keast create namespace spire
keast create cm k8s-workload-registrar -n spire --from-file examples/spire/east/k8s-workload-registrar.conf
keast create cm spire-server -n spire --from-file examples/spire/east/server.conf
keast create cm spire-agent -n spire --from-file examples/spire/east/agent.conf
keast apply -f examples/spire/spire.yaml
kwest create namespace spire
kwest create cm k8s-workload-registrar -n spire --from-file examples/spire/west/k8s-workload-registrar.conf
kwest create cm spire-server -n spire --from-file examples/spire/west/server.conf
kwest create cm spire-agent -n spire --from-file examples/spire/west/agent.conf
kwest apply -f examples/spire/spire.yaml
```

2. Update bundle configs and restart servers:
```shell
spire_bundle_endpoint_west=$(kwest get svc spire-server-bundle-endpoint -n spire -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
sed "s/<spire_bundle_endpoint_west>/$spire_bundle_endpoint_west/g" examples/spire/east/server.conf > examples/spire/east/server-updated.conf
keast delete cm spire-server -n spire
keast create cm spire-server -n spire --from-file=server.conf=examples/spire/east/server-updated.conf
keast rollout restart statefulset spire-server -n spire
keast rollout restart daemonset spire-agent -n spire

spire_bundle_endpoint_east=$(keast get svc spire-server-bundle-endpoint -n spire -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
sed "s/<spire_bundle_endpoint_east>/$spire_bundle_endpoint_east/g" examples/spire/west/server.conf > examples/spire/west/server-updated.conf
kwest delete cm spire-server -n spire
kwest create cm spire-server -n spire --from-file=server.conf=examples/spire/west/server-updated.conf
kwest rollout restart statefulset spire-server -n spire
kwest rollout restart daemonset spire-agent -n spire
```

3. Update bundle endpoints:
```shell
east_bundle=$(keast exec -c spire-server -n spire --stdin spire-server-0  -- /opt/spire/bin/spire-server bundle show -format spiffe -socketPath /run/spire/sockets/server.sock)
west_bundle=$(kwest exec -c spire-server -n spire --stdin spire-server-0  -- /opt/spire/bin/spire-server bundle show -format spiffe -socketPath /run/spire/sockets/server.sock)
keast exec -c spire-server -n spire --stdin spire-server-0 \
  -- /opt/spire/bin/spire-server bundle set -format spiffe -id spiffe://west.local -socketPath /run/spire/sockets/server.sock <<< "$west_bundle"
kwest exec -c spire-server -n spire --stdin spire-server-0 \
  -- /opt/spire/bin/spire-server bundle set -format spiffe -id spiffe://east.local -socketPath /run/spire/sockets/server.sock <<< "$east_bundle"
```

### Install Istio

1. Create certificate:
```shell
keast create namespace istio-system
kwest create namespace istio-system
keast apply -f examples/spire/istio-certificate.yaml -n istio-system
kwest apply -f examples/spire/istio-certificate.yaml -n istio-system
```

2. Install Istio:
```shell
istioctl --kubeconfig=east.kubeconfig install -f examples/spire/east/istio.yaml -y
istioctl --kubeconfig=west.kubeconfig install -f examples/spire/west/istio.yaml -y
```

It does not work, because SPIRE returns empty `trusted_ca` in the validation context, so TLS connections fail.
```shell
istioctl --kubeconfig=east.kubeconfig pc secret deploy/federation-ingress-gateway -n istio-system
```
```shell
RESOURCE NAME     TYPE           STATUS     VALID CERT     SERIAL NUMBER                        NOT AFTER                NOT BEFORE
default           Cert Chain     ACTIVE     true           7189661f911ed11c2c363b24502fc482     2024-10-23T19:17:06Z     2024-10-23T18:16:56Z
ROOTCA                           ACTIVE     false
```
