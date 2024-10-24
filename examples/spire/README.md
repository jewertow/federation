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
keast apply -f examples/spire/crds.yaml
sed -e "s/<cluster_name>/east/g" -e "s/<trust_domain>/east.local/g" examples/spire/spire-quickstart.yaml | keast apply -f -
#keast create cm k8s-workload-registrar -n spire --from-file examples/spire/east/k8s-workload-registrar.conf
#keast create cm spire-server -n spire --from-file examples/spire/east/server.conf
#keast create cm spire-agent -n spire --from-file examples/spire/east/agent.conf
#keast apply -f examples/spire/spire.yaml
kwest create namespace spire
#kwest create cm k8s-workload-registrar -n spire --from-file examples/spire/west/k8s-workload-registrar.conf
#kwest create cm spire-server -n spire --from-file examples/spire/west/server.conf
#kwest create cm spire-agent -n spire --from-file examples/spire/west/agent.conf
#kwest apply -f examples/spire/spire.yaml
kwest apply -f examples/spire/crds.yaml
sed -e "s/<cluster_name>/west/g" -e "s/<trust_domain>/west.local/g" examples/spire/spire-quickstart.yaml | kwest apply -f -
```

Verify registry:
```shell
keast exec -t spire-server-0 -n spire -c spire-server -- ./bin/spire-server entry show
kwest exec -t spire-server-0 -n spire -c spire-server -- ./bin/spire-server entry show
```

2. Create cluster SPIFFE IDs:
```shell
keast apply -f examples/spire/east/cluster-spiffeid.yaml
kwest apply -f examples/spire/west/cluster-spiffeid.yaml
```

3. Deploy Istio:
```shell
keast create namespace istio-system
keast label namespace istio-system spire-identity=true
kwest create namespace istio-system
kwest label namespace istio-system spire-identity=true
istioctl --kubeconfig=east.kubeconfig manifest generate -f examples/spire/east/istio.yaml | keast apply -f -
istioctl --kubeconfig=west.kubeconfig manifest generate -f examples/spire/east/istio.yaml | kwest apply -f -
```

4. Federate bundles:
```shell
spire_bundle_endpoint_west=$(kwest get svc spire-server-bundle-endpoint -n spire -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
west_bundle=$(kwest exec -c spire-server -n spire --stdin spire-server-0  -- /opt/spire/bin/spire-server bundle show -format spiffe -socketPath /tmp/spire-server/private/api.sock)
indented_west_bundle=$(echo "$west_bundle" | jq -r '.' | sed 's/^/    /')
echo -e "  trustDomainBundle: |-\n$indented_west_bundle" >> examples/spire/east/trust-bundle-federation.yaml
sed "s/<west_bundle_endpoints_ip>/$spire_bundle_endpoint_west/g" examples/spire/east/trust-bundle-federation.yaml | keast apply -f -
```
```shell
spire_bundle_endpoint_east=$(keast get svc spire-server-bundle-endpoint -n spire -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
east_bundle=$(keast exec -c spire-server -n spire --stdin spire-server-0  -- /opt/spire/bin/spire-server bundle show -format spiffe -socketPath /tmp/spire-server/private/api.sock)
indented_east_bundle=$(echo "$east_bundle" | jq -r '.' | sed 's/^/    /')
echo -e "  trustDomainBundle: |-\n$indented_east_bundle" >> examples/spire/west/trust-bundle-federation.yaml
sed "s/<east_bundle_endpoints_ip>/$spire_bundle_endpoint_east/g" examples/spire/west/trust-bundle-federation.yaml | kwest apply -f -
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
EAST_INGRESS_IP=$(keast get svc istio-ingressgateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl -v "http://$EAST_INGRESS_IP:80/headers"
```