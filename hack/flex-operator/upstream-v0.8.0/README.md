# Upstream unbounded-operator manifests (v0.8.0)

Rendered from Azure/unbounded `v0.8.0` for drift checks against
`charts/flex-operator`. Regenerate after changing the pinned release:

```bash
git -C unbounded checkout v0.8.0
cd unbounded && go run ./hack/cmd/render-manifests \
  --templates-dir deploy/unbounded-operator --output-dir <out> \
  --set Namespace=unbounded-system \
  --set OperatorImage=ghcr.io/azure/unbounded-operator:v0.8.0 \
  --set ImageRegistry=ghcr.io/azure --set ReapLegacyResources=true
```
