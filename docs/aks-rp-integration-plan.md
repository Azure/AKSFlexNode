# AKS Flex Node Integration with AKS RP - Implementation Plan

## Context
This plan transforms AKS flex-node from a complex, manually-configured solution into a fully Azure-native experience with **one-command setup** and **automatic lifecycle management**. By integrating with AKS Resource Provider (RP), users can deploy and manage third-party edge nodes using standard Azure APIs, CLI, and Portal interfaces.

Currently, aks-flex-node requires complex manual JSON configuration and operates independently after bootstrap. This integration enables:

### Goals

This integration aims to deliver:

- **Dramatically Simplified Setup**: From complex manual configuration with 50+ fields → 2-minute single command
- **Zero Configuration Errors**: All cluster-specific details auto-populated by AKS RP
- **Automatic Lifecycle Management**: Configuration updates, certificate rotation, and version upgrades without manual intervention
- **Azure-Native Experience**: Consistent with existing AKS node pools using familiar Azure tooling
- **Enterprise Ready**: Secure by default with proper authentication, validated configurations, and audit trails

Through three core capabilities:
1. **Auto-Generated Configuration**: AKS RP generates complete flex-node configuration files, eliminating manual complexity
2. **Continuous Configuration Management**: Ongoing lifecycle management through polling-based updates and automatic reconciliation
3. **Standard Azure Integration**: Management through ARM APIs, Azure CLI, Portal, and existing enterprise tooling

## User Experience Transformation

### Before: Complex Manual Setup
```bash
# Current workflow requires multiple manual steps:

# 1. Manually gather cluster information
az aks show --name my-cluster --resource-group my-rg --query "{fqdn: fqdn, location: location}"
az aks get-credentials --name my-cluster --resource-group my-rg --file temp-kubeconfig
kubectl --kubeconfig=temp-kubeconfig config view --raw --minify --flatten

# 2. Create service principal and assign permissions
az ad sp create-for-rbac --name "flex-node-sp" --role "Azure Kubernetes Service Cluster User Role"
az role assignment create --assignee <sp-id> --role "Azure Kubernetes Service Contributor Role"

# 3. Manually construct complex JSON configuration (50+ fields)
cat > flex-node-config.json << EOF
{
  "azure": {
    "subscriptionId": "12345678-1234-1234-1234-123456789abc",
    "tenantId": "87654321-4321-4321-4321-cba987654321",
    "servicePrincipal": {
      "tenantId": "87654321-4321-4321-4321-cba987654321",
      "clientId": "<manual-copy-from-sp-creation>",
      "clientSecret": "<manual-copy-from-sp-creation>"
    },
    "targetCluster": {
      "resourceId": "<manual-copy-from-cluster-info>",
      "location": "<manual-copy-from-cluster-info>"
    }
  },
  "kubernetes": { "version": "<manual-lookup-cluster-version>" },
  "node": {
    "kubelet": {
      "dnsServiceIP": "<manual-extract-from-kubeconfig>",
      "serverURL": "<manual-extract-from-kubeconfig>",
      "caCertData": "<manual-extract-from-kubeconfig>"
    }
  }
  // ... 40+ more fields to populate manually
}
EOF

# 4. Start agent with local config file
aks-flex-node agent --config-file ./flex-node-config.json

# 5. Manual ongoing maintenance
# - Monitor certificate expiration
# - Update config when cluster changes
# - Restart agent for config updates
```

### After: One-Command Setup
```bash
# New integrated workflow - single command:

# 1. Create and start flex node (AKS RP handles everything)
CONFIG_URL=$(az aks flex-node create \
    --cluster my-cluster \
    --resource-group my-rg \
    --name edge-node-1 \
    --auth-method bootstrapToken \
    --output-config-url \
    --query "properties.configUrl" -o tsv)

aks-flex-node agent --config-url "$CONFIG_URL"

# That's it! Agent automatically:
# - Fetches complete auto-generated configuration
# - Joins cluster with proper authentication
# - Starts continuous monitoring for updates
# - Detects and applies configuration changes

# 2. Runtime updates are automatic
az aks flex-node update \
    --cluster my-cluster \
    --resource-group my-rg \
    --node edge-node-1 \
    --kubernetes-version 1.31.0

# Agent detects and applies changes within 30 seconds - zero manual intervention
```

## Complete User Flow

### Architecture Overview - Config URL + Continuous Monitoring

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│   Azure Portal  │────▶│    AKS RP       │◄────│  aks-flex-node  │
│   ARM Template  │     │   FlexNode      │     │                 │
│   CLI/REST API  │     │   Config API    │     │                 │
└─────────────────┘     └─────────────────┘     └─────────────────┘
                               │                          │
                               │                          │ Poll for
                         ┌─────▼──────┐          ┌────────▼──────┐
                         │    Blob    │          │   Config      │
                         │  Storage   │          │  Monitor &    │
                         │ + Config   │          │  Reconcile    │
                         │  Delivery  │          │               │
                         └────────────┘          └───────────────┘
```

### Phase 1: Initial Setup via Config URL
```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   Customer      │    │   AKS RP        │    │ aks-flex-node   │
│   (Portal/CLI)  │    │                 │    │   (on edge)     │
└─────┬───────────┘    └─────┬───────────┘    └─────┬───────────┘
      │                      │                      │
  ┌───▼────────────────────────────────────────────────────────┐
  │ 1. Customer: Create FlexNode config and get config URL     │
  │    az aks flex-node create --cluster my-cluster            │
  │    --resource-group my-rg --name edge-1                    │
  │    --auth-method bootstrapToken --output-url               │
  └───┬────────────────────────────────────────────────────────┘
      │                      │                      │
      │                  ┌───▼────────────────────────────────────┐
      │                  │ 2. AKS RP: Generate config + temp URL  │
      │                  │    - Bootstrap token created           │
      │                  │    - Complete config generated         │
      │                  │    - Store in blob with SAS token      │
      │                  │    - Return temp URL (tied to token)   │
      │                  └───┬────────────────────────────────────┘
      │                      │                      │
  ┌───▼────────────────────────────────────────────────────────────┐
  │ 3. Customer: Start agent with config URL from the edge device  │
  │    aks-flex-node agent --config-url $TEMP_URL                  │
  └───┬────────────────────────────────────────────────────────────┘
      │                      │                      │
      │                      │                  ┌───▼─────────────┐
      │                      │                  │ 4. Agent: Fetch │
      │                      │                  │    config, boot-│
      │                      │                  │    strap & join │
      │                      │                  │    cluster      │
      │                      │                  └───┬─────────────┘
```

### Phase 2: Continuous Configuration Monitoring (Optional)
```
      │                      │                      │
      │                      │                  ┌───▼─────────────┐
      │                      │                  │ 5. Agent: Start │
      │                      │                  │    polling loop │
      │                      │                  │   (30s interval)│
      │                      │                  └───┬─────────────┘
      │                      │                      │
      │                      │                  ┌───▼─────────────┐
      │                      │                  │ 6. Agent: Poll  │
      │                      │◄─────────────────┤    for config   │
      │                      │                  │     updates     │
      │                      │                  └───┬─────────────┘
      │                      │                      │
      │                  ┌───▼────────────────────────────────────┐
      │                  │ 7. AKS RP: Return config + version     │
      │                  │    configVersion: "1708123456"         │
      │                  └───┬────────────────────────────────────┘
```

### Phase 3: Runtime Updates
```
      │                      │                      │
  ┌───▼────────────────────────────────────────────────────────────┐
  │ 8. Customer: Update config via CLI (anytime)                   │
  │     az aks flex-node update --cluster my-cluster               │
  │     --node edge-1 --kubernetes-version 1.31.0                  │
  └───┬────────────────────────────────────────────────────────────┘
      │                      │                      │
      │                  ┌───▼────────────────────────────────────────────┐
      │                  │ 9. AKS RP: Update config                       │
      │                  │     configVersion: "1708123456" → "1708127056" │
      │                  └───┬────────────────────────────────────────────┘
      │                      │                      │
      │                      │                  ┌───▼─────────────┐
      │                      │                  │ 10. Agent: Auto-│
      │                      │◄─────────────────┤     detect &    │
      │                      │  (next poll cycle)     reconcile   │
      │                      │                  └───┬─────────────┘
      │                      │                      │
  ┌───▼──────────────────────────────────────────────▼─────────────┐
  │ 11. Result: Node automatically updated to new config           │
  └────────────────────────────────────────────────────────────────┘
```

## Three Key Technical Components

### 1. Auto-Generated Configuration

#### Current Configuration Complexity

The aks-flex-node currently requires a comprehensive JSON configuration file with multiple complex sections:

- **Azure Configuration**: Subscription ID, tenant ID, cluster resource ID, authentication method
- **Node Configuration**: Kubelet settings, DNS service IP, API server URL, CA certificates
- **Component Versions**: Kubernetes, containerd, runc, CNI, NPD versions
- **System Paths**: Various Kubernetes component paths and directories

#### AKS RP Auto-Generation Capabilities

**AKS RP has access to all cluster information needed to auto-generate most configuration:**

##### ✅ **Auto-Generatable Fields:**
1. **Azure Context** (from cluster resource):
   - Subscription ID, tenant ID, target cluster resource ID and location
2. **Kubernetes Context** (from cluster configuration):
   - Kubernetes version, DNS service IP, API server URL, CA certificate
3. **Component Versions** (AKS-compatible versions):
   - Default containerd, runc, CNI, NPD versions tested with the cluster

##### 🔧 **User-Provided Fields:**
- Authentication method choice (Bootstrap Token/Service Principal/Managed Identity)
- Node-specific settings (max pods, custom labels)
- Optional version overrides for components

### 2. Node Bootstrap Process

#### Dual Authentication Architecture

The agent requires two separate authentication mechanisms:

1. **Kubernetes Authentication**: For kubelet to join the cluster
2. **Azure Authentication**: For ongoing AKS RP communication

Authentication Method Support

| Authentication Method | Kubernetes Authentication | Azure Authentication |
|----------------------|---------------------------|---------------------|
| **Bootstrap Token** | Works with all AKS clusters (one-time use for node joining) | Not supported |
| **Service Principal** | Works with AAD-integrated clusters | Works on any hardware |
| **Managed Identity (Azure VM/VMSS)** | Works with AAD-integrated clusters | Works on Azure VMs/VMSS only |
| **Azure Arc** | Works with AAD-integrated clusters (via Arc machine identity) | Works on Arc-enabled machines (including non-Azure hardware) |

### 3. Continuous Monitoring & Self-Reconciliation (Optional)

**Note**: Continuous monitoring is optional and configurable. Users can choose bootstrap-only mode for simpler setups without ongoing Azure authentication.

#### Configuration Modes

**Bootstrap-Only Mode**:
- Use bootstrap token for initial node joining only
- No ongoing AKS RP communication after bootstrap
- Simpler setup with no Azure authentication requirements
- Node operates independently after initial cluster join

**Continuous Monitoring Mode**:
- Requires Azure authentication (Service Principal, Managed Identity, or Azure Arc)
- Enables ongoing configuration updates and lifecycle management
- Provides operational visibility and automated reconciliation
- Supports remote configuration changes via AKS RP

#### Supported Operations

The AKS RP integration introduces a new ARM resource type: **flexNodeConfig**.

**FlexNodeConfig Resource Overview:**
The `flexNodeConfig` is a new ARM resource that represents the configuration and lifecycle management for a single flex node. It serves as the bridge between Azure Resource Manager and the aks-flex-node agent running on edge hardware.

**Key Characteristics:**
- **ARM Resource Type**: `Microsoft.ContainerService/managedClusters/flexNodeConfig`
- **Scope**: Sub-resource of an AKS managed cluster
- **Purpose**: Stores configuration, authentication details, and operational state for one flex node
- **Lifecycle**: Independent of the physical machine - can exist before/after the actual hardware
- **Management**: Fully managed through standard Azure APIs, CLI, Portal, and ARM templates

**Resource Relationship:**
```
AKS Managed Cluster (parent)
├── Agent Pools (existing)
├── FlexNode Configs (new) ← One per flex node
│   ├── edge-node-1
│   ├── edge-node-2
│   └── edge-node-N
└── Other cluster resources
```

**What flexNodeConfig Contains:**
- Authentication configuration (bootstrap tokens, service principals, managed identity)
- Node-specific settings (labels, kubelet configuration, resource limits)
- Component versions (Kubernetes, containerd, runtime versions)
- Operational state (last seen, health status, configuration version)

**What flexNodeConfig Does NOT Contain:**
- The actual physical/virtual machine
- Runtime state of the node (that's in Kubernetes)
- Hardware-specific configuration

Based on the technical constraints of the agent architecture, the following operations are realistically supported for flexNodeConfig resources:

##### Supported Operations

1. **Create/Update Operations**

   `PUT .../flexNodeConfig/{nodeName}`
   - FlexNodeConfig resource creation and updates
   - Node configuration changes (labels, taints, kubelet settings)
   - Kubernetes version upgrades (with controlled kubelet restart)
   - Container runtime updates (containerd/runc)
   - Authentication credential updates

2. **Get/List Operations**

   | Endpoint | Purpose | Audience |
   |----------|---------|----------|
   | `GET .../flexNodeConfig/{nodeName}` | Full resource with config + status | Customers & Agents |
   | `GET .../flexNodeConfig` | List all resources with metadata + status | Customers |

3. **Delete Operations**

   `DELETE .../flexNodeConfig/{nodeName}`
   - FlexNodeConfig ARM resource cleanup and removal
   - Bootstrap token and config URL revocation
   - Monitoring termination (stops accepting polls from deleted node)
   - Does NOT affect the physical machine or remove node from Kubernetes cluster

## API Design & Implementation Details

### FlexNode Configuration API

The FlexNode configuration API replaces complex manual configuration with AKS RP-generated configurations that include all cluster-specific details.

#### FlexNode Config Resource Creation and Updates

**API Design Note: CREATE vs UPDATE Semantics**
The FlexNode Config API follows ARM's idempotent PUT pattern where the same endpoint handles both resource creation and updates. The RP implementation must:
- Return **201 Created** for new resource creation
- Return **200 OK** for existing resource updates
- Handle `configUrl` regeneration policy: **Always regenerate** config URLs on PUT operations (both create and update) to ensure fresh bootstrap credentials and maintain security best practices
- Maintain **full resource schema** for all PUT operations (no partial updates) to ensure consistent ARM behavior

**Example: Service Principal for Both Kubernetes and Azure Auth (AAD Cluster)**
```http
PUT /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}?api-version=2026-05-01-preview&configUrl=true

Authorization: Bearer {customer-azure-token}
Content-Type: application/json

{
  "properties": {
    "continuousMonitoring": true,
    "azureAuth": {
      "method": "servicePrincipal",
      "servicePrincipal": {
        "clientId": "11111111-2222-3333-4444-555555555555",
        "clientSecret": "your-secret-here",
        "clientTenant": "your-tenant-here"
      }
    },
    "kubernetesAuth": {
      "useAzureAuth": true
    },
    "versions": {
      "kubernetes": "1.30.0",
      "containerd": "1.7.1"
    },
    "nodeConfig": {
      "maxPods": 50,
      "labels": {
        "environment": "prod"
      }
    }
  }
}

Response 201 Created:
{
  "properties": {
    "provisioningState": "Succeeded",
    "configVersion": "1708123456",
    "configUrl": "https://aksconfigs.blob.core.windows.net/configs/{nodeName}-config.json?sv=2022-11-02&ss=b&srt=o&sp=r&se=2024-02-27T10:00:00Z&sig=xyz123",
    "configUrlExpires": "2024-02-27T10:00:00Z"  // Revoked when bootstrap token consumed
  }
}
```

#### Configuration Bundle (Retrieved from Config URL)

**Example: Continuous Monitoring Mode**
```http
GET https://aksconfigs.blob.core.windows.net/configs/{nodeName}-config.json?sv=2022-11-02&...

Response:
{
  "config": {
    "continuousMonitoring": true,
    "azure": {
      "subscriptionId": "auto-populated-from-cluster",
      "tenantId": "auto-populated-from-subscription",
      "servicePrincipal": {
        "clientId": "11111111-2222-3333-4444-555555555555",
        "clientSecret": "your-secret-here",
        "clientTenant": "your-tenant-here"
      },
      "targetCluster": {
        "resourceId": "/subscriptions/.../managedClusters/cluster-name",
        "location": "cluster-location"
      }
    },
    "kubernetes": {
      "version": "1.30.0"
    },
    "node": {
      "maxPods": 50,
      "kubelet": {
        "dnsServiceIP": "cluster-dns-service-ip",
        "serverURL": "cluster-api-server-url",
        "caCertData": "cluster-ca-cert-base64"
      }
    }
    // ... rest of complete configuration
  }
}
```

**Agent Polling Logic:**
The agent constructs the monitoring URL using the cluster information from the config:
- **Pattern**: `/subscriptions/{subscriptionId}/resourceGroups/{resourceGroup}/providers/Microsoft.ContainerService/managedClusters/{clusterName}/flexNodeConfig/{nodeName}`
- **Authentication**: Uses the Azure credentials provided in the config

#### Configuration Updates (Customer-Initiated)

**Example: k8s version Update**
```http
PUT /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}?api-version=2026-05-01-preview

Authorization: Bearer {customer-azure-token}
Content-Type: application/json

{
  "properties": {
    "continuousMonitoring": true,
    "azureAuth": {
      "method": "servicePrincipal",
      "servicePrincipal": {
        "clientId": "11111111-2222-3333-4444-555555555555",
        "clientSecret": "your-secret-here",
        "clientTenant": "your-tenant-here"
      }
    },
    "kubernetesAuth": {
      "useAzureAuth": true
    },
    "versions": {
      "kubernetes": "1.31.0",  // Updated version
      "containerd": "1.7.2"    // Updated version
    },
    "nodeConfig": {
      "maxPods": 50,
      "labels": {
        "environment": "prod"
      }
    }
  }
}
```

**Response 200 OK:**
```json
{
  "name": "edge-node-1",
  "properties": {
    "provisioningState": "Succeeded",
    "configVersion": "1708127056",
    "continuousMonitoring": true,
    "azureAuth": {
      "method": "servicePrincipal",
      "servicePrincipal": {
        "clientId": "11111111-2222-3333-4444-555555555555",
        // clientSecret omitted from responses for security
        "clientTenant": "your-tenant-here"
      }
    },
    "kubernetesAuth": {
      "useAzureAuth": true
    },
    "versions": {
      "kubernetes": "1.31.0",  // Updated version applied
      "containerd": "1.7.2"    // Updated version applied
    },
    "nodeConfig": {
      "maxPods": 50,
      "labels": {
        "environment": "prod"
      }
    }
  }
}
```

#### Configuration and Status Retrieval

Version-Based Change Detection with Jittered Polling

The continuous monitoring system uses version-based change detection with distributed polling to prevent thundering herd scenarios:

- **Config Versioning**: Each configuration has a Unix timestamp-based version with natural ordering (e.g., `configVersion: "1708123456"`)
- **Jittered Polling**: Agent polls at "30 seconds ± random(0-10s)" intervals to distribute load
  - **Base Interval**: 30 seconds target polling frequency
  - **Jitter Range**: ±10 second random offset per agent (33% jitter)
  - **Effective Range**: 20-40 second actual polling intervals
  - **Load Distribution**: Prevents all agents from polling simultaneously during RP restarts or network recovery
- **Change Detection**: When version changes (`"1708123456" → "1708127056"`), agent knows config updated
- **Efficiency**: Agent only needs to compare timestamp strings, not entire config content

**Single Resource Endpoint - Contains both configuration and status information:**
```http
GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}?api-version=2026-05-01-preview

Authorization: Bearer {customer-or-agent-azure-token}

Response:
{
  "name": "edge-node-1",
  "properties": {
    "provisioningState": "Succeeded",
    "configVersion": "1708123456",

    // Configuration (for agents)
    "config": {
      "azure": {
        "subscriptionId": "auto-populated",
        "tenantId": "auto-populated",
        "targetCluster": {
          "resourceId": "auto-populated",
          "location": "auto-populated"
        }
      },
      "kubernetes": {
        "version": "1.31.0"
      },
      "node": {
        "maxPods": 110,
        "kubelet": {
          "dnsServiceIP": "cluster-dns-service-ip",
          "serverURL": "cluster-api-server-url",
          "caCertData": "cluster-ca-cert-base64"
        }
      }
      // ... full configuration without sensitive auth details
    },

    // Status
    "status": {
      "conditions": [
        {
          "type": "Ready",
          "status": "True",
          "lastTransitionTime": "2024-02-26T15:30:45Z"
        }
      ],
      "lastConfigApplied": "2024-02-26T14:00:00Z",
      "appliedConfigVersion": "1708123456"
    }
  }
}
```

#### FlexNode Config Resource Deletion

**DELETE Operation for Resource Cleanup**
```http
DELETE /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}?api-version=2026-05-01-preview

Authorization: Bearer {customer-azure-token}

Response 200 OK:
{
  "status": "Deleted",
  "cleanup": {
    "configUrlRevoked": true,
    "bootstrapTokenRevoked": true,
    "monitoringDisabled": true
  },
  "note": "ARM resource deleted. Node machine lifecycle (stop/start/destroy) must be managed separately."
}
```

**DELETE Operation Behavior:**
- **ARM Resource Cleanup**: Removes the flexNodeConfig ARM resource from Azure Resource Manager
- **Credential Revocation**: Invalidates any associated bootstrap tokens and config URLs
- **Monitoring Termination**: Stops accepting monitoring polls from the deleted node
- **Agent Unbootstrap Trigger**: Agent detects missing flexNodeConfig during next poll cycle and initiates unbootstrap sequence
- **Agent Cleanup Responsibilities**: Agent removes node from Kubernetes cluster, cleans up Azure resources (Arc machine identity, etc.), and stops local services
- **Node Machine Preservation**: Does NOT stop, restart, or destroy the actual physical/virtual machine hardware

**Important Notes:**
- **Graceful Unbootstrap**: Agent automatically detects flexNodeConfig deletion and performs graceful cleanup
- **Cleanup Sequence**: Agent removes itself from Kubernetes cluster → cleans up Azure RBAC permissions → stops services → remains available for re-bootstrap
- **Physical Machine Preservation**: Hardware/VM remains running and available for potential re-joining with new flexNodeConfig
- **Re-bootstrap Capability**: Node can rejoin cluster by creating a new flexNodeConfig resource

#### FlexNode Config List Operation

```http
GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig?api-version=2026-05-01-preview

Authorization: Bearer {customer-azure-token}

Response 200 OK:
{
  "value": [
    {
      "name": "edge-node-1",
      "properties": {
        "provisioningState": "Succeeded",
        "configVersion": "1708123456",
        "status": {
          "conditions": [
            {
              "type": "Ready",
              "status": "True",
              "lastTransitionTime": "2024-02-26T15:30:45Z"
            }
          ],
          "lastConfigApplied": "2024-02-26T14:00:00Z",
          "appliedConfigVersion": "1708123456"
        }
      }
    },
    {
      "name": "edge-node-2",
      "properties": {
        "provisioningState": "Succeeded",
        "configVersion": "1708120000",
        "status": {
          "conditions": [
            {
              "type": "Ready",
              "status": "False",
              "lastTransitionTime": "2024-02-26T10:00:00Z",
              "reason": "KubeletNotReady"
            }
          ],
          "lastConfigApplied": "2024-02-26T08:00:00Z",
          "appliedConfigVersion": "1708120000"
        }
      }
    }
  ]
}
```

**Benefits:**
- **Dashboard Ready**: List provides status overview perfect for monitoring dashboards
- **ARM Consistent**: Standard ARM list pattern with embedded resource status
- **Single API Surface**: Reduces API complexity while providing full functionality

## Frequently Asked Questions

### Q: Why does the agent need a config URL for initial startup, but call AKS RP directly afterwards?

**A: This solves the bootstrap authentication challenge for third-party nodes.**

The agent faces a chicken-and-egg problem:
- **Initial Problem**: Agent on non-Azure hardware has no Azure identity to authenticate with AKS RP
- **Config URL Solution**: Customer creates config via authenticated CLI/Portal, gets temporary SAS URL with embedded credentials
- **After Bootstrap**: Agent uses the credentials from initial config to authenticate directly with AKS RP for ongoing monitoring

### Q: How does drift detection work if someone manually changes node configuration?

**A: The agent maintains local state and can detect/reconcile drift.**

**Agent Local State:**
```
/var/lib/aks-flex-node/
├── current-config.json          # Latest config from AKS RP
├── applied-config.json          # What was actually applied to system
├── config-version.txt           # Version for polling efficiency
```

**Drift Detection Scenarios:**

1. **AKS RP Config Changes** (normal flow):
   - Agent polls AKS RP, sees new `configVersion`
   - Downloads new config, compares with `current-config.json`
   - Applies changes, updates both local files

2. **Manual System Changes**:
   - Agent periodically validates critical system state against `applied-config.json`
   - **Detectable Changes**: kubelet config files, systemd units, container runtime settings, certificates
   - **Detection Methods**: File checksums, systemctl status checks, process validation
   - **Response**: Log drift events, optionally reconcile by reapplying configuration from AKS RP

3. **Agent Restart**:
   - Agent reads `current-config.json` and `config-version.txt`
   - Resumes polling from last known version
   - Ensures no configuration loss during downtime

### Q: How are bootstrap token failures and retries handled?

**A: The RP implements automatic token reissue for failed bootstrap scenarios.**

**Bootstrap Token Lifecycle:**
- **Initial Token**: Generated on flexNodeConfig creation, valid for 24 hours, single-use
- **Config URL Lifetime**: Tied to bootstrap token consumption - both become invalid once bootstrap is attempted
- **Consumption Trigger**: Token consumed on first kubelet authentication attempt (successful or failed)
- **Automatic Revocation**: Config URL is immediately revoked when bootstrap token is consumed, regardless of join success

**Failure Scenarios & Recovery:**

1. **Agent Crash Before Join**:
   ```
   Agent fetches config → crashes → restarts → token still valid → proceeds normally
   ```

2. **Agent Crash After Bootstrap Attempt**:
   ```
   Agent starts join → token consumed → config URL revoked → crashes → restarts → BOTH INVALID
   ```
   **Recovery**: Agent must request new bootstrap token + config URL via re-PUT operation

3. **Network Issues During Join**:
   ```
   Agent starts join → network timeout → token consumed → config URL revoked → RETRY FAILS
   ```
   **Recovery**: Both credentials invalid - request new token + config URL

**Token Reissue Flow:**
```http
# Customer detects join failure, requests new token via ARM action
POST /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}/regenerateToken?api-version=2026-05-01-preview

Authorization: Bearer {customer-azure-token}
Content-Type: application/json

{}

# RP generates fresh bootstrap token and new config URL
Response 200 OK:
{
  "configVersion": "1708127890",
  "configUrl": "https://aksconfigs.blob.core.windows.net/configs/{nodeName}-config-v2.json?...",
  "configUrlExpires": "2024-02-27T11:00:00Z"  // Revoked when bootstrap token consumed
}
```

**Agent Implementation Requirements:**
- **Persist Join State**: Track whether cluster join was successful
- **Detect Join Failures**: Identify when bootstrap token was consumed but join failed
- **Request New Tokens**: Surface clear error messages directing users to regenerate config
- **Graceful Degradation**: Continue operating if already joined, even if monitoring fails

**RP Implementation Requirements:**
- **Token State Tracking**: Monitor bootstrap token consumption and join success
- **Automatic Cleanup**: Clean up unused/expired tokens and config URLs
- **Action Endpoint**: Support `/regenerateToken` POST action for explicit reissue scenarios
