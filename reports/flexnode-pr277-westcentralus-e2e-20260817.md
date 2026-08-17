# FlexNode PR #277 West Central US E2E Report

## Status

- Overall result: **PASS WITH RP CUSTOM-LABEL ISOLATION FINDING**
- Current phase: complete
- Started: `2026-08-17T18:53:51Z`
- Last updated: `2026-08-17T20:02:00Z`

## Scope

- Subscription: `8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8`
- Region: `westcentralus`
- Target regional AKS RP release: `v20260807`
- Agent changes:
  - PR [#275](https://github.com/Azure/AKSFlexNode/pull/275): AKS-owned node labels
  - PR [#276](https://github.com/Azure/AKSFlexNode/pull/276): strict Machine goal responses
  - PR [#277](https://github.com/Azure/AKSFlexNode/pull/277): authoritative Machine goals
- Agent branch: `wenx/authoritative-machine-goals`
- Agent commit: `3c0aa0397c81c66cd190332a2b9dff7ead3f7cea`
- Test host: real Azure VM with system-assigned managed identity
- Control path: Azure CLI `aks-preview` FlexNodes commands

## Test Objectives

1. Create an AKS cluster and FlexNodes pool using Azure CLI.
2. Pre-create an RP Machine whose goal deliberately differs from pool bootstrap/local config.
3. Start the PR #277 agent on a real Azure VM and prove the existing RP Machine goal is authoritative.
4. Verify Machine custom labels contain only customer labels and exclude these AKS-owned labels:
   - `kubernetes.azure.com/managed`
   - `kubernetes.azure.com/agentpool`
   - `kubernetes.azure.com/mode`
   - `kubernetes.azure.com/nodepool-type`
5. Verify the Kubernetes Node contains the four AKS-owned labels plus the Machine custom labels.
6. Verify the effective Machine goal controls Kubernetes version, `maxPods`, labels, taints, and persisted daemon state.
7. Restart the nspawn node/agent and prove the authoritative applied goal survives restart.

## Test Constraints

- This test does not validate workload networking. The external VM is not provisioned by the AKS RP and may not receive a production CNI configuration.
- Secret responses such as bootstrap tokens and CA data are never written to this report.
- Commands are recorded with non-secret identifiers. Sensitive response fields are reduced to booleans or redacted summaries.

## Execution Log

### Step 1: Verify Local Source and Tooling

- Result: **PASS**

Command:

```bash
git status --short --branch
git log --oneline --decorate --max-count=8
git rev-parse HEAD
```

Response:

```text
## wenx/authoritative-machine-goals...origin/wenx/authoritative-machine-goals
3c0aa03 Align E2E Machine max pods with node config
44b711f Adopt authoritative AKS Machine goals
072fe11 Validate AKS Machine goal responses (#276)
981f1cd Add AKS-owned labels to Flex Nodes (#275)
3c0aa0397c81c66cd190332a2b9dff7ead3f7cea
```

Command:

```bash
az version
```

Response, relevant fields:

```json
{
  "azure-cli": "2.86.0",
  "extensions": {
    "aks-preview": "22.0.0b1"
  }
}
```

### Step 2: Verify Subscription and Preview Features

- Result: **PASS**

Command:

```bash
az account show \
  --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --query '{id:id,name:name,state:state,tenantId:tenantId,userType:user.type}' \
  -o json
```

Response:

```json
{
  "id": "8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8",
  "name": "Azure Container Service - Test (AKS Standalone)",
  "state": "Enabled",
  "tenantId": "72f988bf-86f1-41af-91ab-2d7cd011db47",
  "userType": "user"
}
```

Commands:

```bash
az feature show --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --namespace Microsoft.ContainerService --name AKSFlexNodePreview \
  --query '{name:name,state:properties.state}' -o json

az feature show --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --namespace Microsoft.ContainerService --name PutMachinePreview \
  --query '{name:name,state:properties.state}' -o json
```

Responses:

```json
{"name":"Microsoft.ContainerService/AKSFlexNodePreview","state":"Registered"}
{"name":"Microsoft.ContainerService/PutMachinePreview","state":"Registered"}
```

### Step 3: Verify Regional Kubernetes and VM Capacity

- Result: **PASS**

Command:

```bash
az aks get-versions \
  --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --location westcentralus \
  --query 'values[*].patchVersions.keys(@)[]' -o json
```

Response summary:

```text
Available versions include 1.35.7 and 1.36.3.
Selected test version: 1.35.7.
```

Commands:

```bash
az vm list-skus --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --location westcentralus --size Standard_D4s_v5 \
  --resource-type virtualMachines -o json

az vm list-skus --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --location westcentralus --size Standard_D2s_v5 \
  --resource-type virtualMachines -o json

az vm list-usage --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --location westcentralus -o json
```

Response summary:

```text
Standard_D4s_v5: unrestricted, 4 vCPU, 16 GiB
Standard_D2s_v5: unrestricted, 2 vCPU, 8 GiB
Standard DSv5 quota: 100 vCPU available
Regional total quota: 2300 vCPU available
```

### Step 4: Verify Azure CLI FlexNodes Command Surface

- Result: **PASS**

Commands:

```bash
az aks nodepool add -h
az aks machine add -h
az aks nodepool get-bootstrap-data -h
```

Response summary:

```text
az aks nodepool add supports --vm-set-type FlexNodes.
az aks machine add supports machine name, Kubernetes version, max pods, labels, and taints.
az aks nodepool get-bootstrap-data is available from aks-preview.
```

### Step 5: Build the PR Agent Artifact

- Result: **PASS**
- Release publication decision: no public alpha release was needed; the exact PR binary will be copied to the test VM.

Command:

```bash
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
  -ldflags "-X github.com/Azure/AKSFlexNode/pkg/cmd/version.Version=v0.1.7-alpha.277 \
  -X github.com/Azure/AKSFlexNode/pkg/cmd/version.GitCommit=3c0aa0397c81c66cd190332a2b9dff7ead3f7cea \
  -X github.com/Azure/AKSFlexNode/pkg/cmd/version.BuildTime=${BUILD_DATE} -w -s" \
  -o /tmp/opencode/fn277-wcu-artifacts/aks-flex-node-linux-amd64 \
  ./cmd/aks-flex-node

tar -C /tmp/opencode/fn277-wcu-artifacts -czf \
  /tmp/opencode/fn277-wcu-artifacts/aks-flex-node-linux-amd64.tar.gz \
  aks-flex-node-linux-amd64

sha256sum /tmp/opencode/fn277-wcu-artifacts/aks-flex-node-linux-amd64.tar.gz
/tmp/opencode/fn277-wcu-artifacts/aks-flex-node-linux-amd64 version
```

Response:

```text
47909047b39973069cb9ecc5f60cfae83429deb5b8a8df42bf605540ad759bfe  aks-flex-node-linux-amd64.tar.gz
AKS Flex Node Agent
Version: v0.1.7-alpha.277
Git Commit: 3c0aa0397c81c66cd190332a2b9dff7ead3f7cea
Build Time: 2026-08-17T19:05:31Z
```

### Step 6: Create the Azure Resource Group and Network

- Result: **PASS**

Commands:

```bash
az account set --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8

az group create --name fn277-wcu-20260817 --location westcentralus \
  --tags purpose=flexnode-pr277-e2e owner=wenxuan pr=277 \
  agentCommit=3c0aa03 rpRelease=v20260807

az network vnet create -g fn277-wcu-20260817 -n fn277-vnet \
  --location westcentralus --address-prefixes 10.247.0.0/16 \
  --subnet-name aks-subnet --subnet-prefixes 10.247.0.0/22

az network nsg create -g fn277-wcu-20260817 -n fn277-vm-nsg \
  --location westcentralus

az network vnet subnet create -g fn277-wcu-20260817 \
  --vnet-name fn277-vnet --name flex-subnet \
  --address-prefixes 10.247.4.0/24 --network-security-group fn277-vm-nsg
```

Response summary:

```text
Resource group provisioning: Succeeded
VNet: 10.247.0.0/16
AKS subnet: 10.247.0.0/22
Flex VM subnet: 10.247.4.0/24
```

### Step 7: Create the AKS Cluster

- Result: **PASS**

Command:

```bash
az aks create \
  --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --resource-group fn277-wcu-20260817 \
  --name fn277wcu \
  --location westcentralus \
  --kubernetes-version 1.35.7 \
  --nodepool-name systempool \
  --node-count 1 \
  --node-vm-size Standard_D2s_v5 \
  --network-plugin azure \
  --network-plugin-mode overlay \
  --vnet-subnet-id <aks-subnet-resource-id> \
  --service-cidr 10.248.0.0/16 \
  --dns-service-ip 10.248.0.10 \
  --enable-managed-identity \
  --ssh-key-value ~/.ssh/id_rsa.pub
```

Response, relevant fields:

```json
{
  "currentVersion": "1.35.7",
  "location": "westcentralus",
  "name": "fn277wcu",
  "networkPlugin": "azure",
  "networkPluginMode": "overlay",
  "state": "Succeeded",
  "systemPool": {
    "count": 1,
    "name": "systempool",
    "state": "Succeeded",
    "vmSize": "Standard_D2s_v5"
  }
}
```

### Step 8: Create the FlexNodes Pool and Inspect Bootstrap Data

- Result: **PASS**

Command:

```bash
az aks nodepool add \
  --resource-group fn277-wcu-20260817 \
  --cluster-name fn277wcu \
  --name flexpool \
  --vm-set-type FlexNodes \
  --mode User \
  --kubernetes-version 1.35.7 \
  --max-pods 75 \
  --labels pool-source=bootstrap-stale remove-after-bootstrap=true \
  --node-taints pool-source=bootstrap-stale:NoSchedule \
  --max-unavailable 30%
```

Response, relevant fields:

```json
{
  "currentVersion": "1.35.7",
  "labels": {
    "pool-source": "bootstrap-stale",
    "remove-after-bootstrap": "true"
  },
  "maxPods": 75,
  "mode": "User",
  "name": "flexpool",
  "state": "Succeeded",
  "taints": ["pool-source=bootstrap-stale:NoSchedule"],
  "type": "FlexNodes"
}
```

Command:

```bash
az aks nodepool get-bootstrap-data \
  -g fn277-wcu-20260817 --cluster-name fn277wcu -n flexpool \
  --query '{targetPool:azure.targetAgentPoolName,kubernetes:components.kubernetes,maxPods:node.maxPods,labels:node.labels,taints:node.taints,hasToken:length(azure.bootstrapToken.token) > `0`,hasCACert:length(node.kubelet.caCertData) > `0`}' \
  -o json
```

Sanitized response:

```json
{
  "hasCACert": true,
  "hasToken": true,
  "kubernetes": "1.35.7",
  "labels": {
    "pool-source": "bootstrap-stale",
    "remove-after-bootstrap": "true"
  },
  "maxPods": 75,
  "taints": ["pool-source=bootstrap-stale:NoSchedule"],
  "targetPool": "flexpool"
}
```

Observation: pool bootstrap data contains only the configured customer labels. It does not contain the four AKS-owned labels.

### Step 9: Pre-create the Authoritative RP Machine

- Result: **PASS WITH RP RESPONSE OBSERVATION**

Command:

```bash
az aks machine add \
  -g fn277-wcu-20260817 \
  --cluster-name fn277wcu \
  --nodepool-name flexpool \
  --machine-name fn277vm \
  --kubernetes-version 1.35.7 \
  --max-pods 61 \
  --labels source=machine-authoritative machine-only=true \
  --node-taints source=machine-authoritative:NoSchedule
```

Response, relevant fields:

```json
{
  "name": "fn277vm",
  "properties": {
    "eTag": "91f22a47-dee8-4ab4-ba67-337abdb82b76",
    "kubernetes": {
      "currentOrchestratorVersion": "1.35.7",
      "maxPods": 61,
      "nodeLabels": {
        "kubernetes.azure.com/managed": "false",
        "machine-only": "true",
        "source": "machine-authoritative"
      },
      "nodeName": "fn277vm",
      "nodeTaints": ["source=machine-authoritative:NoSchedule"],
      "orchestratorVersion": "1.35.7"
    },
    "provisioningState": "Succeeded"
  }
}
```

Observation: the CLI request contained only `source` and `machine-only`; the RP response added `kubernetes.azure.com/managed=false`. `machine show` and `machine list` returned the same expanded label map. The remaining three AKS-owned labels were not present in the ARM Machine response.

The Kubernetes Node did not exist before VM bootstrap:

```text
Error from server (NotFound): nodes "fn277vm" not found
```

### Step 10: Create the Real Azure VM and Assign AKS Access

- Result: **PASS**

Commands:

```bash
az network nsg rule create -g fn277-wcu-20260817 \
  --nsg-name fn277-vm-nsg --name AllowSSHFromOperator \
  --priority 100 --direction Inbound --access Allow --protocol Tcp \
  --source-address-prefixes 67.168.38.253/32 \
  --destination-port-ranges 22

az vm create \
  -g fn277-wcu-20260817 -n fn277vm --location westcentralus \
  --image Ubuntu2404 --size Standard_D4s_v5 \
  --admin-username azureuser --ssh-key-values ~/.ssh/id_rsa.pub \
  --vnet-name fn277-vnet --subnet flex-subnet --nsg "" \
  --assign-identity --public-ip-sku Standard --os-disk-size-gb 64 \
  --security-type TrustedLaunch
```

Response summary:

```text
VM private IP: 10.247.4.4
VM public IP: 20.168.179.165
VM state: running
System-assigned principal: a7183dd9-4799-4b8b-b09a-10889758d431
```

Command:

```bash
az role assignment create \
  --assignee-object-id a7183dd9-4799-4b8b-b09a-10889758d431 \
  --assignee-principal-type ServicePrincipal \
  --role "Azure Kubernetes Service Contributor Role" \
  --scope <fn277wcu-resource-id>
```

Response summary:

```text
Role assignment ID: d03807f8-20da-4369-a646-8dd218d41122
Scope: AKS cluster fn277wcu
```

### Step 11: Stage and Verify the PR Agent on the VM

- Result: **PASS**

Commands:

```bash
scp -i ~/.ssh/id_rsa \
  /tmp/opencode/fn277-wcu-artifacts/aks-flex-node-linux-amd64.tar.gz \
  azureuser@20.168.179.165:/tmp/aks-flex-node-linux-amd64.tar.gz

scp -i ~/.ssh/id_rsa scripts/bootstrap.sh \
  azureuser@20.168.179.165:/tmp/bootstrap.sh

ssh -i ~/.ssh/id_rsa azureuser@20.168.179.165 \
  'sha256sum /tmp/aks-flex-node-linux-amd64.tar.gz; bash -n /tmp/bootstrap.sh'

ssh -i ~/.ssh/id_rsa azureuser@20.168.179.165 \
  'curl -fsS -H Metadata:true "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F" | jq -r '"'"'if (.access_token | length) > 0 then "token-acquired" else "missing-token" end'"'"''
```

Response:

```text
47909047b39973069cb9ecc5f60cfae83429deb5b8a8df42bf605540ad759bfe  /tmp/aks-flex-node-linux-amd64.tar.gz
token-acquired
```

An initial artifact-verification command ran concurrently with the copy and observed the files before transfer completion. The serial retry above passed; no bootstrap mutation had started.

### Step 12: Bootstrap with Deliberately Conflicting Local Settings

- Result: **PASS**

Command, non-secret form:

```bash
sudo bash /tmp/bootstrap.sh \
  --auth msi \
  --fetch-bootstrap-data \
  --cluster-resource-id <fn277wcu-resource-id> \
  --agent-pool-name flexpool \
  --agent-url file:///tmp/aks-flex-node-linux-amd64.tar.gz \
  --agent-sha256 47909047b39973069cb9ecc5f60cfae83429deb5b8a8df42bf605540ad759bfe \
  --config-overrides '{
    "agent":{"nodeName":"fn277vm","logLevel":"debug"},
    "node":{
      "maxPods":47,
      "labels":{"pool-source":"bootstrap-overridden","local-only":"true"},
      "taints":["local-source=bootstrap-stale:NoSchedule"],
      "kubelet":{
        "nodeIP":"10.247.4.4",
        "imageGCHighThreshold":90,
        "imageGCLowThreshold":75
      }
    }
  }'
```

Response summary:

```text
bootstrap: fetching fresh bootstrap data from AKS RP
bootstrap: rendered config at /etc/aks-flex-node/config.json
preflight: all required checks passed
level=INFO msg="machine already registered, adopting remote goal"
level=INFO msg="operation completed successfully" operation=bootstrap
```

Installed binary:

```text
Version: v0.1.7-alpha.277
Git Commit: 3c0aa0397c81c66cd190332a2b9dff7ead3f7cea
```

Service state:

```text
aks-flex-node-agent.service: active, enabled
nspawn machine: kube1
kubelet: active
containerd: active
```

### Step 13: Validate Initial RP Machine Authority

- Result: **PASS WITH RP CUSTOM-LABEL ISOLATION FINDING**

The persisted local config intentionally remains stale:

```json
{
  "configMaxPods": 47,
  "configLabels": {
    "local-only": "true",
    "pool-source": "bootstrap-overridden",
    "remove-after-bootstrap": "true"
  },
  "configTaints": ["local-source=bootstrap-stale:NoSchedule"],
  "imageGC": {"high": 90, "low": 75},
  "kubernetes": "1.35.7",
  "nodeIP": "10.247.4.4"
}
```

The daemon state adopted the RP Machine goal:

```json
{
  "appliedGoal": {
    "kubernetesVersion": "1.35.7",
    "settingsVersion": "91f22a47-dee8-4ab4-ba67-337abdb82b76",
    "maxPods": 61,
    "nodeLabels": {
      "kubernetes.azure.com/managed": "false",
      "machine-only": "true",
      "source": "machine-authoritative"
    },
    "nodeTaints": ["source=machine-authoritative:NoSchedule"],
    "kubeletConfig": {
      "imageGCHighThreshold": 90,
      "imageGCLowThreshold": 75
    }
  },
  "activeMachine": "kube1"
}
```

Generated kubelet settings:

```text
maxPods: 61
imageGCHighThresholdPercent: 90
imageGCLowThresholdPercent: 75
--node-ip=10.247.4.4
--node-labels=kubernetes.azure.com/agentpool=flexpool,kubernetes.azure.com/managed=false,kubernetes.azure.com/mode=user,kubernetes.azure.com/nodepool-type=FlexNodes,machine-only=true,source=machine-authoritative
--register-with-taints=source=machine-authoritative:NoSchedule
```

Kubernetes Node result:

```json
{
  "uid": "78179c06-2b60-4cb5-aeee-f16dadad3fed",
  "maxPods": "61",
  "kubeletVersion": "v1.35.7",
  "serverOwnedLabels": {
    "kubernetes.azure.com/managed": "false",
    "kubernetes.azure.com/agentpool": "flexpool",
    "kubernetes.azure.com/mode": "user",
    "kubernetes.azure.com/nodepool-type": "FlexNodes"
  },
  "machineLabels": {
    "source": "machine-authoritative",
    "machine-only": "true"
  },
  "staleLabels": {
    "pool-source": null,
    "remove-after-bootstrap": null,
    "local-only": null
  }
}
```

The Machine taint was present; stale pool/local taints were absent. The Node registered but remained `Ready=False` because no CNI configuration was installed on this externally provisioned VM. Workload networking was intentionally outside this test scope.

Conclusions:

1. The pre-created RP Machine, not pool bootstrap data or local config overrides, controlled Kubernetes version, `maxPods`, labels, and taints.
2. The agent added all four AKS-owned labels to the Node.
3. The original CLI request did not specify any AKS-owned label. However, the RP create/show/list response injected `kubernetes.azure.com/managed=false` into `properties.kubernetes.nodeLabels`, and the agent persisted that returned value in `appliedGoal.nodeLabels`. Therefore strict server-owned/custom-label separation does **not** hold in the current westcentralus RP response contract.
4. The other three AKS-owned labels were absent from ARM Machine labels and added only at kubelet registration.

### Step 14: Validate Restart Persistence

- Result: **PASS**

Commands:

```bash
sudo systemctl restart systemd-nspawn@kube1.service
sudo systemctl -M kube1 is-active kubelet containerd
sudo systemctl restart aks-flex-node-agent.service
sudo systemctl is-active aks-flex-node-agent.service
```

Response:

```text
kubelet: active
containerd: active
aks-flex-node-agent.service: active
```

Daemon state checksum before and after restart:

```text
de7638b2c4764e3d865261668ef4cae994583e8da7a3174cecebfc21f16aed33
de7638b2c4764e3d865261668ef4cae994583e8da7a3174cecebfc21f16aed33
```

Post-restart assertions:

```text
Node UID unchanged: 78179c06-2b60-4cb5-aeee-f16dadad3fed
Active nspawn machine unchanged: kube1
Machine ETag unchanged: 91f22a47-dee8-4ab4-ba67-337abdb82b76
Node maxPods unchanged: 61
Kubelet version unchanged: v1.35.7
Machine custom labels and taint preserved
All four AKS-owned Node labels preserved
Stale pool/local metadata remained absent
```

The daemon repeatedly selected `ReportSucceeded` after restart, with no goal apply or repave.

### Step 15: Inspect Regional Cluster Integration

- Result: **INFORMATIONAL**

Commands:

```bash
kubectl api-resources --api-group=unbounded-cloud.io -o wide
kubectl get clusterrole,clusterrolebinding -l kubernetes.azure.com/managedby=aks -o name
```

Response summary:

```text
No unbounded-cloud.io MachineOperation API was installed.
No dedicated AKS Flex daemon RBAC was present.
The agent logged: Machina MachineOperation API not found; using noop machine operation reconciler.
```

This means the regional test can validate authoritative bootstrap, daemon state, and restart persistence, but not PR #4's future in-place acknowledgement path.

### Step 16: Update the RP Machine Goal

- Result: **PASS**

Command:

```bash
az aks machine update \
  -g fn277-wcu-20260817 \
  --cluster-name fn277wcu \
  --nodepool-name flexpool \
  --machine-name fn277vm \
  --labels source=machine-updated update-only=true \
  --node-taints source=machine-updated:NoExecute
```

Response, relevant fields:

```json
{
  "name": "fn277vm",
  "properties": {
    "eTag": "30f40af4-ef1a-4969-a06d-dfe46e80b2d5",
    "kubernetes": {
      "currentOrchestratorVersion": "1.35.7",
      "maxPods": 61,
      "nodeLabels": {
        "kubernetes.azure.com/managed": "false",
        "source": "machine-updated",
        "update-only": "true"
      },
      "nodeTaints": ["source=machine-updated:NoExecute"],
      "orchestratorVersion": "1.35.7"
    },
    "provisioningState": "Succeeded"
  }
}
```

Before deleting the Node:

```text
Live Node UID remained 78179c06-2b60-4cb5-aeee-f16dadad3fed.
Live Node retained source=machine-authoritative and machine-only=true.
Daemon applied ETag remained 91f22a47-dee8-4ab4-ba67-337abdb82b76.
Agent decision changed to WaitForNodeSignal.
```

Conclusion: PR #277 correctly did not acknowledge or apply the changed goal in place. That behavior remains in the separate PR #4 scope.

### Step 17: Trigger and Validate Blue-Green Repave

- Result: **PASS WITH ONE TRANSIENT RETRY**

Command:

```bash
kubectl delete node fn277vm --wait=false
```

Response:

```text
node "fn277vm" deleted
```

Observed transition:

```text
19:42:36 active=kube1 appliedETag=91f22a47-... node=absent
19:43:21 active=kube2 appliedETag=30f40af4-... nodeUID=a5bbaf76-... source=machine-updated update-only=true maxPods=61
```

Agent log sequence:

```text
decision=WaitForNodeSignal reason="goal state differs but node deletion trigger is absent"
decision=ApplyGoalState reason="node deletion observed and goal state is not applied"
refresh bootstrap data for repave: ... dial tcp ... i/o timeout
decision=ApplyGoalState reason="node deletion observed and goal state is not applied"
refreshed AKS bootstrap data for repave
starting nspawn machine goal-state apply oldMachine=kube1 newMachine=kube2 settingsVersion=30f40af4-... kubernetesVersion=1.35.7
completed task=save-daemon-state status=ok
```

The first `listBootstrapData` call hit a transient ARM network timeout. Controller-runtime retried immediately and the second call succeeded without manual intervention.

Final daemon state:

```json
{
  "appliedGoal": {
    "kubernetesVersion": "1.35.7",
    "settingsVersion": "30f40af4-ef1a-4969-a06d-dfe46e80b2d5",
    "maxPods": 61,
    "nodeLabels": {
      "kubernetes.azure.com/managed": "false",
      "source": "machine-updated",
      "update-only": "true"
    },
    "nodeTaints": ["source=machine-updated:NoExecute"],
    "kubeletConfig": {
      "imageGCHighThreshold": 90,
      "imageGCLowThreshold": 75
    }
  },
  "previousAppliedGoal": {
    "kubernetesVersion": "1.35.7",
    "settingsVersion": "91f22a47-dee8-4ab4-ba67-337abdb82b76",
    "maxPods": 61,
    "nodeLabels": {
      "kubernetes.azure.com/managed": "false",
      "machine-only": "true",
      "source": "machine-authoritative"
    },
    "nodeTaints": ["source=machine-authoritative:NoSchedule"]
  },
  "activeMachine": "kube2"
}
```

Final Kubernetes Node:

```json
{
  "uid": "a5bbaf76-07f7-497c-bbb8-9d0b57c266c8",
  "maxPods": "61",
  "kubeletVersion": "v1.35.7",
  "labels": {
    "kubernetes.azure.com/managed": "false",
    "kubernetes.azure.com/agentpool": "flexpool",
    "kubernetes.azure.com/mode": "user",
    "kubernetes.azure.com/nodepool-type": "FlexNodes",
    "source": "machine-updated",
    "update-only": "true",
    "machine-only": null,
    "pool-source": null,
    "local-only": null
  },
  "taint": "source=machine-updated:NoExecute"
}
```

The recreated Node remained `Ready=False` only because the externally provisioned VM had no CNI configuration.

## Test Matrix

| Test | Result | Evidence |
| --- | --- | --- |
| Azure CLI FlexNodes pool create/show/bootstrap | PASS | `flexpool` Succeeded; bootstrap data returned stale pool goal |
| Azure CLI Machine create/show/list | PASS | synchronous Machine with ETag `91f22a47-...` |
| Real Azure VM + MSI bootstrap | PASS | PR binary installed; AKS role scoped to cluster |
| Existing RP Machine authoritative at first bootstrap | PASS | local maxPods 47 vs applied/Node maxPods 61; Machine metadata won |
| Stale pool/local labels and taints excluded from Node | PASS | all deliberately stale keys absent |
| Four AKS-owned labels present on Node | PASS | managed, agentpool, mode, nodepool-type verified |
| CLI Machine create request used customer labels only | PASS | Request specified only `source` and `machine-only` |
| Server-owned labels absent from Machine custom-label response | **FAIL / RP FINDING** | RP injected `kubernetes.azure.com/managed=false` into Machine `nodeLabels` |
| Complete applied goal persistence | PASS | daemon state stores Machine ETag, maxPods, labels, taints, GC thresholds |
| Legacy scalar projections written | PASS | applied settings/Kubernetes fields match full goal |
| Nspawn and daemon restart persistence | PASS | state checksum and Node UID/settings unchanged |
| Machine update waits for Node deletion | PASS | agent selected `WaitForNodeSignal` |
| Machine goal repave | PASS | kube1→kube2, new ETag, new UID, updated metadata |
| Previous applied goal rotation | PASS | old full goal retained in `previousAppliedGoal` |
| Bootstrap-data refresh on repave | PASS WITH RETRY | first ARM call timed out; automatic retry succeeded |
| Workload networking / Node Ready | NOT IN SCOPE | no CNI installed on external VM |
| In-place label/taint acknowledgement | NOT AVAILABLE | regional cluster lacks MachineOperation CRD; PR #4 is separate |

## Findings

1. **RP custom-label isolation:** `az aks machine add` requested only `source` and `machine-only`, but create/show/list/raw ARM responses included `kubernetes.azure.com/managed=false` under `properties.kubernetes.nodeLabels`. The agent therefore persisted that RP-returned label in its complete applied goal. The other three AKS-owned labels remained outside the Machine response and were added only at kubelet registration.
2. **No regional MachineOperation integration:** westcentralus did not install `unbounded-cloud.io` CRDs or dedicated Flex daemon RBAC for this cluster, so PR #4 acknowledgement could not be tested.
3. **External VM CNI:** the Node remains `Ready=False` with `NetworkPluginNotReady`. This was expected because workload networking was outside the requested goal-authority test and the VM is not RP-provisioned.
4. **Transient ARM timeout:** one repave bootstrap-data refresh timed out, then succeeded through the existing reconciliation retry.

## Overall Result

PRs #275, #276, and #277 passed the requested real-VM goal-authority validation:

- the existing RP Machine controlled first bootstrap despite conflicting pool/local config;
- the exact RP `maxPods`, labels, taints, version, and ETag reached kubelet, daemon state, and the Node;
- all four AKS-owned labels were present on the Node;
- full applied state survived restart;
- a real CLI Machine update was applied through blue-green repave and rotated the previous complete goal.

The only requested assertion that did not hold was strict server-owned/custom-label separation in the ARM Machine response because the westcentralus RP adds `kubernetes.azure.com/managed=false` to `properties.kubernetes.nodeLabels`.

## Test Resources

| Resource | Planned value |
| --- | --- |
| Resource group | `fn277-wcu-20260817` |
| AKS cluster | `fn277wcu` |
| System pool | `systempool` |
| FlexNodes pool | `flexpool` |
| Azure VM / Machine / Node | `fn277vm` |
| VNet | `fn277-vnet` |
| AKS subnet | `aks-subnet` |
| Flex VM subnet | `flex-subnet` |
| Kubernetes version | `1.35.7` |
| Pool max pods | `75` |
| Machine max pods | `61` |
| Initial Machine custom labels | `source=machine-authoritative`, `machine-only=true` |
| Initial Machine taint | `source=machine-authoritative:NoSchedule` |
| Deliberately stale local labels | `pool-source=bootstrap-overridden`, `local-only=true` |

## Cleanup

- Result: **PASS**

Commands:

```bash
az group delete \
  --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --name fn277-wcu-20260817 \
  --yes --no-wait

az group wait \
  --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --name fn277-wcu-20260817 \
  --deleted --interval 15 --timeout 1800

az group exists \
  --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --name fn277-wcu-20260817

az resource list \
  --subscription 8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8 \
  --tag purpose=flexnode-pr277-e2e \
  -o json
```

Responses:

```text
Resource group exists: false
Tagged resources remaining: []
```
