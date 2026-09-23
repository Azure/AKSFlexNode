# AKS Flex Config Helper

<a id="prerequisites"></a>
<a id="save-the-helper"></a>
<a id="shared-cluster-arguments"></a>
<a id="setup-node-rbac"></a>
<a id="generate-node-config"></a>
<a id="bootstrap-token"></a>
<a id="managed-identity"></a>
<a id="service-principal"></a>
<a id="azure-arc"></a>
<a id="copy-to-host"></a>

`scripts/aks-flex-config` is a repository development helper for rendering standalone configuration during internal evaluation. It is not part of the current Flex node onboarding workflow.

For supported onboarding, use the version-matched bootstrap script with `--fetch-bootstrap-data` as shown in [Bootstrap an AKS Flex Node](getting-started.md). AKS provides the pool-issued Kubernetes bootstrap data and manages the associated bootstrap and CSR integration; operators don't need to create client-side bootstrap RBAC or tokens.

For restricted environments, use the bootstrap script's rootfs and offline-artifact override flags described in the [offline bootstrap lab](../labs/aks-public-cluster-offline-bootstrap.md). This keeps identity, bootstrap-data retrieval, and Machine registration on the same pool-scoped flow as online onboarding.

The helper remains in the repository for development compatibility. Its generated files can contain credentials and must not be printed, logged, committed, or stored in shared locations.
