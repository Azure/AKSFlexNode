# Kube-Proxy Behavior For HBC / Flex Nodes

This note records a kube-proxy behavior observed while validating KubeVirt on AKS Flex Nodes with `unbounded-net`.

## Scenario

Cluster shape:

- Public AKS cluster.
- AKS Flex Nodes in another VNet connected by VNet peering.
- AKS cluster created with `--network-plugin none`.
- `unbounded-net` provides CNI and creates a site-scoped managed kube-proxy DaemonSet for the Flex site.
- `SitePeering` uses `meshNodes: true` and `tunnelProtocol: Auto`.
- KubeVirt is installed from the upstream manifests.

Example CIDRs from validation:

```text
AKS VNet:       10.111.0.0/16
Flex VNet:      10.112.0.0/16
AKS pod CIDR:   10.113.0.0/16
Service CIDR:   10.114.0.0/16
Flex pod CIDR:  10.115.0.0/16
```

The Kubernetes service in the cluster was:

```text
kubernetes.default service IP: 10.114.0.1
kubernetes.default endpoint:   20.165.13.240:443
```

The endpoint is the public AKS API server IP.

## Symptom

KubeVirt installed successfully and the AKS-hosted KubeVirt components became ready, but `virt-handler` on Flex Nodes stayed unready.

Flex `virt-handler` logs showed API watch/list calls timing out against the Kubernetes service IP:

```text
failed to list *v1.KubeVirt: Get "https://10.114.0.1:443/apis/kubevirt.io/v1/kubevirts?...": dial tcp 10.114.0.1:443: i/o timeout
failed to list *v1.Node: Get "https://10.114.0.1:443/api/v1/nodes?...": dial tcp 10.114.0.1:443: i/o timeout
```

A test pod scheduled on a Flex Node reproduced the same behavior:

```bash
curl -k --connect-timeout 5 https://10.114.0.1:443/
```

Result:

```text
Failed to connect to 10.114.0.1 port 443: Timeout was reached
```

From the Flex node host / `kube1` host namespace, direct access to both the service IP and the public endpoint worked:

```text
curl https://10.114.0.1:443  -> HTTP 401 Unauthorized
curl https://20.165.13.240:443 -> HTTP 401 Unauthorized
```

So the problem was specific to pod-source traffic through kube-proxy service NAT.

## What Was Working

The issue was not a general overlay failure.

Validated working paths:

- AKS node private IP to Flex node private IP.
- Flex node private IP to AKS node private IP.
- AKS pod to Flex pod.
- Flex pod to AKS pod.
- Flex pod to Flex pod.
- KubeVirt device exposure into the nspawn machine:
  - `/dev/kvm`
  - `/dev/net/tun`
  - `/dev/vhost-net`

The KubeVirt devices were present in the nspawn machine and allowed by the systemd device cgroup:

```text
Bind=/dev/kvm
Bind=/dev/net/tun
Bind=/dev/vhost-net

DeviceAllow=/dev/kvm rwm
DeviceAllow=/dev/net/tun rwm
DeviceAllow=/dev/vhost-net rwm
```

## Diagnosis

On the Flex Node, the unbounded-managed kube-proxy programmed the Kubernetes service like this:

```text
-A KUBE-SERVICES -d 10.114.0.1/32 -p tcp \
  -m comment --comment "default/kubernetes:https cluster IP" \
  -m tcp --dport 443 \
  -j KUBE-SVC-NPX46M4PTMTKRN6Y

-A KUBE-SVC-NPX46M4PTMTKRN6Y ! -s 10.115.0.0/16 -d 10.114.0.1/32 -p tcp \
  -m comment --comment "default/kubernetes:https cluster IP" \
  -m tcp --dport 443 \
  -j KUBE-MARK-MASQ

-A KUBE-SVC-NPX46M4PTMTKRN6Y \
  -m comment --comment "default/kubernetes:https -> 20.165.13.240:443" \
  -j KUBE-SEP-TNQOO5K4G5TFAHOT

-A KUBE-SEP-TNQOO5K4G5TFAHOT -p tcp \
  -m comment --comment "default/kubernetes:https" \
  -m tcp \
  -j DNAT --to-destination 20.165.13.240:443
```

The important rule is:

```text
! -s 10.115.0.0/16 ... -j KUBE-MARK-MASQ
```

Because the source was a Flex pod IP inside `10.115.0.0/16`, kube-proxy did **not** mark the packet for masquerade.

The packet was DNATed to the public API server endpoint but kept the pod source IP:

```text
src: 10.115.x.x
dst: 20.165.13.240:443
```

That source IP is not routable from the public AKS API endpoint, so the return path failed and the connection timed out.

## Confirmation

Adding a temporary MASQ rule on the Flex Node made Flex pod to Kubernetes service traffic succeed immediately:

```bash
iptables -t nat -I KUBE-SVC-NPX46M4PTMTKRN6Y 1 \
  -s 10.115.0.0/16 \
  -d 10.114.0.1/32 \
  -p tcp \
  --dport 443 \
  -j KUBE-MARK-MASQ
```

After this rule was added:

```bash
curl -k https://10.114.0.1:443/
```

returned the expected unauthenticated API response:

```text
HTTP/2 401
{
  "kind": "Status",
  "status": "Failure",
  "message": "Unauthorized",
  "reason": "Unauthorized",
  "code": 401
}
```

This confirms the failure is caused by missing SNAT for pod-source traffic to the public AKS API endpoint through the `kubernetes.default` service.

## Why This Matters For KubeVirt

`virt-handler` runs as a pod on each node and uses in-cluster Kubernetes client configuration. On Flex Nodes, that means it talks to:

```text
https://kubernetes.default.svc
```

which resolves to the Kubernetes service IP. If Flex pod traffic to the Kubernetes service times out, `virt-handler` cannot watch/list KubeVirt and Kubernetes resources, so it never becomes ready even though `/dev/kvm`, `/dev/net/tun`, and `/dev/vhost-net` are exposed correctly.

## Candidate Fix

The unbounded-managed kube-proxy for Flex sites should ensure service traffic from Flex pod CIDRs to external/public service endpoints is masqueraded.

Possible approaches:

1. Run the managed kube-proxy with masquerading behavior that covers this case, such as `--masquerade-all=true` if compatible with the rest of the networking model.
2. Add targeted masquerade handling for `default/kubernetes` when its endpoint is outside the node/pod routable networks.
3. More generally, SNAT pod-source service traffic when the selected endpoint is external to the unbounded pod/node routing domain.

A manual patch to the generated kube-proxy DaemonSet is not durable because the unbounded controller reconciles the managed kube-proxy object.

## Current Workaround

For ad hoc validation only, add a temporary rule on each Flex Node for the Kubernetes service chain:

```bash
iptables -t nat -I <KUBE-SVC-CHAIN-FOR-KUBERNETES> 1 \
  -s <flex-pod-cidr> \
  -d <kubernetes-service-ip>/32 \
  -p tcp \
  --dport 443 \
  -j KUBE-MARK-MASQ
```

Example from validation:

```bash
iptables -t nat -I KUBE-SVC-NPX46M4PTMTKRN6Y 1 \
  -s 10.115.0.0/16 \
  -d 10.114.0.1/32 \
  -p tcp \
  --dport 443 \
  -j KUBE-MARK-MASQ
```

This is not persistent and should not be treated as the product fix.

## Useful Debug Commands

Inspect the Kubernetes service endpoint:

```bash
kubectl get svc kubernetes -o wide
kubectl get endpoints kubernetes -o wide
```

Inspect kube-proxy service NAT rules from inside the Flex nspawn machine:

```bash
leader=$(machinectl show kube1 -p Leader --value)
nsenter -t "$leader" -m -u -i -n -p -- \
  iptables-save -t nat | grep -E 'default/kubernetes|<kubernetes-service-ip>|<api-endpoint-ip>|KUBE-MARK-MASQ'
```

Test service and endpoint reachability from the Flex node namespace:

```bash
leader=$(machinectl show kube1 -p Leader --value)
nsenter -t "$leader" -m -u -i -n -p -- \
  curl -k -sS --connect-timeout 5 https://<kubernetes-service-ip>:443/
nsenter -t "$leader" -m -u -i -n -p -- \
  curl -k -sS --connect-timeout 5 https://<api-endpoint-ip>:443/
```

Test from a Flex pod:

```bash
kubectl run api-smoke \
  --image=curlimages/curl:8.8.0 \
  --restart=Never \
  --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"<flex-node-name>"},"tolerations":[{"operator":"Exists"}]}}' \
  --command -- sh -c 'sleep 3600'

kubectl wait --for=condition=Ready pod/api-smoke --timeout=180s
kubectl exec api-smoke -- curl -k --connect-timeout 5 https://<kubernetes-service-ip>:443/
```
