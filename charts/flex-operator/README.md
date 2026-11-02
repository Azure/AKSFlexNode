# flex-operator Helm chart

> [!NOTE]
> Experimental. This chart packages the Unbounded operator so that AKS can
> install and upgrade it as a managed cluster extension. It isn't a supported
> installation method yet; use `kubectl unbounded install` as described in the
> [getting-started guide](../../docs/usage/getting-started.md).

The chart wraps the upstream
[`deploy/unbounded-operator`](https://github.com/Azure/unbounded/tree/v0.8.0/deploy/unbounded-operator)
manifests at the release pinned in `Chart.yaml` (`appVersion`). It installs
the same ServiceAccount, ConfigMap, ClusterRole, ClusterRoleBinding, and
Deployment that `kubectl unbounded install` applies. The operator then installs
its CRDs at startup and reconciles components from Sites.

## Install

The chart must be installed into the `unbounded-system` namespace; rendering
into any other namespace fails.

```bash
helm install flex-operator charts/flex-operator \
  --namespace unbounded-system --create-namespace --wait
kubectl -n unbounded-system rollout status deploy/unbounded-operator
```

## Values

| Value | Default | Description |
| --- | --- | --- |
| `operator.imageRegistry` | `ghcr.io/azure` | Image prefix for the operator and every operator-managed component. |
| `operator.image` | `unbounded-operator:<appVersion>` | Operator `name:tag` without a registry. |
| `operator.imagePullPolicy` | `IfNotPresent` | The tag is pinned, so the upstream `Always` isn't needed. |
| `operator.apiServerEndpoint` | `""` | API server endpoint advertised to remote machines. Empty lets the operator discover it. |
| `operator.reapLegacyResources` | `true` | Upstream legacy-migration switch. |
| `sites`, `sitePeering` | empty | Optional Sites and SitePeering applied by a post-install/post-upgrade Job. |

A Site with networking enabled makes the operator install Unbounded-Net, which
replaces the cluster CNI. Set `sites` only on an AKS cluster created with
`--network-plugin none`. See [values.yaml](values.yaml) for an example.

## Differences from the upstream manifests

| Area | Upstream | Chart |
| --- | --- | --- |
| Namespace | Rendered as an object | Release namespace, must be `unbounded-system` |
| Operator image | Stamped by `kubectl unbounded install` | `operator.imageRegistry` and `operator.image` |
| `imagePullPolicy` | `Always` | `IfNotPresent` |
| Config hash annotation | Hash of the CLI's JSON | Hash of the rendered ConfigMap data |
| Sites | `kubectl unbounded site init` | Optional `sites` value |

Check for unintended drift after changing the chart or the pinned release:

```bash
helm template flex-operator charts/flex-operator -n unbounded-system \
  | python3 hack/flex-operator/compare_upstream.py - hack/flex-operator/upstream-v0.8.0
```

## Uninstall

`helm uninstall` removes the operator, its RBAC, and its ConfigMap. It leaves
the CRDs the operator installed, any Sites, and the component workloads that
the operator reconciled. Delete Flex node pools and Sites before uninstalling.

## Open questions

- Who owns CRD, Site, and component cleanup when the operator is removed.
- Whether Sites belong in this chart or remain with `kubectl unbounded site init` and AKS.
- How to detect and adopt an operator installed with `kubectl unbounded install`.
- Image mirroring for the operator and every component under one registry prefix.
