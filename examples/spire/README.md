### Install Cert Manager

```shell
helm repo add jetstack https://charts.jetstack.io --force-update
```
```shell
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

#### Create cluster issuer

1. Generate root certificates for both clusters:
```shell
make -f Makefile.selfsigned.mk \
  ROOTCA_CN="East Root CA" \
  ROOTCA_ORG=my-company.org \
  root-ca
mkdir -p east
mv root-cert.pem east
mv root-key.pem east
make -f common.mk clean

make -f Makefile.selfsigned.mk \
  ROOTCA_CN="West Root CA" \
  ROOTCA_ORG=my-company.org \
  root-ca
mkdir -p west
mv root-cert.pem west
mv root-key.pem west
make -f common.mk clean
```

2. Create cluster issuers from generated certificates:
```shell
keast create secret tls ca-key-cert --cert=east/root-cert.pem --key=east/root-key.pem -n cert-manager
kwest create secret tls ca-key-cert --cert=west/root-cert.pem --key=west/root-key.pem -n cert-manager
keast apply -f examples/spire/cluster-issuer.yaml
kwest apply -f examples/spire/cluster-issuer.yaml
```

### Install SPIRE

1. Install SPIRE:
```shell
keast create namespace spire
keast apply -f examples/spire/crds.yaml
sed "s/<cluster_name>/east/g" examples/spire/spire.yaml | keast apply -f -
kwest create namespace spire
kwest apply -f examples/spire/crds.yaml
sed "s/<cluster_name>/west/g" examples/spire/spire.yaml | kwest apply -f -
```

2. Create cluster SPIFFE IDs:
```shell
sed "s/<remote_cluster>/west/g" examples/spire/cluster-spiffeid.yaml | keast apply -f -
sed "s/<remote_cluster>/east/g" examples/spire/cluster-spiffeid.yaml | kwest apply -f -
```

3. Federate bundles:
```shell
spire_bundle_endpoint_west=$(kwest get svc spire-server-bundle-endpoint -n spire -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
west_bundle=$(kwest exec -c spire-server -n spire --stdin spire-server-0  -- /opt/spire/bin/spire-server bundle show -format spiffe -socketPath /tmp/spire-server/private/api.sock)
indented_west_bundle=$(echo "$west_bundle" | jq -r '.' | sed 's/^/    /')
echo -e "  trustDomainBundle: |-\n$indented_west_bundle" >> examples/spire/east/trust-bundle-federation.yaml
sed "s/<remote_bundle_endpoint_ip>/$spire_bundle_endpoint_west/g" examples/spire/east/trust-bundle-federation.yaml | keast apply -f -
```
```shell
spire_bundle_endpoint_east=$(keast get svc spire-server-bundle-endpoint -n spire -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
east_bundle=$(keast exec -c spire-server -n spire --stdin spire-server-0  -- /opt/spire/bin/spire-server bundle show -format spiffe -socketPath /tmp/spire-server/private/api.sock)
indented_east_bundle=$(echo "$east_bundle" | jq -r '.' | sed 's/^/    /')
echo -e "  trustDomainBundle: |-\n$indented_east_bundle" >> examples/spire/west/trust-bundle-federation.yaml
sed "s/<remote_bundle_endpoint_ip>/$spire_bundle_endpoint_east/g" examples/spire/west/trust-bundle-federation.yaml | kwest apply -f -
```

4. Deploy Istio:
```shell
keast create namespace istio-system
keast label namespace istio-system spire-identity=true
kwest create namespace istio-system
kwest label namespace istio-system spire-identity=true
sed -e "s/<local_cluster_name>/east/g" -e "s/<remote_cluster_name>/west/g" examples/spire/istio.yaml | istioctl --kubeconfig=east.kubeconfig manifest generate -f - | keast apply -f -
sed -e "s/<local_cluster_name>/west/g" -e "s/<remote_cluster_name>/east/g" examples/spire/istio.yaml | istioctl --kubeconfig=west.kubeconfig manifest generate -f - | kwest apply -f -
```
Verify Spire's registry:
```shell
keast exec -t spire-server-0 -n spire -c spire-server -- ./bin/spire-server entry show
kwest exec -t spire-server-0 -n spire -c spire-server -- ./bin/spire-server entry show
```

5. Deploy federation controllers:
```shell
WEST_GATEWAY_IP=$(kwest get svc federation-ingress-gateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
helm-east install east-mesh chart -n istio-system \
    --values examples/federation-controller.yaml \
    --set "federation.meshPeers.remote.addresses[0]=$WEST_GATEWAY_IP" \
    --set "federation.configMode=k8s" \
    --set "istio.sdsProvider=spire"
EAST_GATEWAY_IP=$(keast get svc federation-ingress-gateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
helm-west install west-mesh chart -n istio-system \
    --values examples/federation-controller.yaml \
    --set "federation.meshPeers.remote.addresses[0]=$EAST_GATEWAY_IP" \
    --set "federation.configMode=k8s" \
    --set "istio.sdsProvider=spire"
```

6. Deploy and export apps:
```shell
keast label namespace default istio-injection=enabled
keast label namespace default spire-identity=true
keast apply -f examples/spire/east/sleep.yaml
keast apply -f examples/mtls.yaml -n istio-system
kwest label namespace default istio-injection=enabled
kwest label namespace default spire-identity=true
kwest apply -f examples/spire/west/httpbin.yaml
kwest label service httpbin export-service=true
kwest apply -f examples/mtls.yaml -n istio-system
```

7. Send a test request:
```shell
keast exec deploy/sleep -c sleep -- curl -v httpbin.default.svc.cluster.local:8000/headers
```
Expected response:
```
> GET /headers HTTP/1.1
> Host: httpbin.default.svc.cluster.local:8000
> User-Agent: curl/8.10.1
> Accept: */*
> 
* Request completely sent off
< HTTP/1.1 200 OK
< access-control-allow-credentials: true
< access-control-allow-origin: *
< content-type: application/json; charset=utf-8
< date: Thu, 24 Oct 2024 19:52:12 GMT
< content-length: 627
< x-envoy-upstream-service-time: 0
< server: envoy
< 
{ [627 bytes data]
100   627  100   627    0     0   317k      0 --:--:-- --:--:-- --:--:--  612k
* Connection #0 to host httpbin.default.svc.cluster.local left intact
{
  "headers": {
    "Accept": [
      "*/*"
    ],
    "Host": [
      "httpbin.default.svc.cluster.local:8000"
    ],
    "User-Agent": [
      "curl/8.10.1"
    ],
    "X-Envoy-Attempt-Count": [
      "1"
    ],
    "X-Forwarded-Client-Cert": [
      "By=spiffe://west.local/ns/default/sa/httpbin;Hash=49d0778341d0807c13439f203387a780d5110791d859aa1358364b283f018b51;Subject=\"x500UniqueIdentifier=3976473ba59715fdcaaeba3e5b4c6bda,O=SPIRE,C=US\";URI=spiffe://east.local/ns/default/sa/sleep"
    ],
    "X-Forwarded-Proto": [
      "http"
    ],
    "X-Request-Id": [
      "f90e877c-97ce-4e1d-8b3c-8bc4b3d10988"
    ]
  }
}
```

8. Configure the east ingress gateway and check if traffic was routed to httpbin in the west cluster:
```shell
keast apply -f examples/spire/east/httpbin-gateway.yaml
```
```shell
EAST_INGRESS_IP=$(keast get svc istio-ingressgateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl -v "http://$EAST_INGRESS_IP:80/headers"
```