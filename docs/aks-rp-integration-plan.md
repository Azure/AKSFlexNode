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
# - Handles certificate rotation automatically

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
                         │+ Config    │          │  Reconciler   │
                         │ Delivery   │          │               │
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
      │                  │    - Return temp URL (24h expiry)      │
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
      │                      │  (agent checks for updates)        │
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

### Auto-Generated Configuration

#### Current Configuration Complexity

The aks-flex-node currently requires a comprehensive JSON configuration file with multiple complex sections:

- **Azure Configuration**: Subscription ID, tenant ID, cluster resource ID, authentication method
- **Node Configuration**: Kubelet settings, DNS service IP, API server URL, CA certificates
- **Component Versions**: Kubernetes, containerd, runc, CNI, NPD versions
- **System Paths**: Various Kubernetes component paths and directories

**Example Current Manual Configuration:**
```json
{
  "azure": {
    "subscriptionId": "12345678-1234-1234-1234-123456789abc",
    "tenantId": "87654321-4321-4321-4321-cba987654321",
    "servicePrincipal": {
      "tenantId": "87654321-4321-4321-4321-cba987654321",
      "clientId": "11111111-2222-3333-4444-555555555555",
      "clientSecret": "your-secret-here"
    },
    "targetCluster": {
      "resourceId": "/subscriptions/12345678-1234-1234-1234-123456789abc/resourceGroups/my-rg/providers/Microsoft.ContainerService/managedClusters/my-cluster",
      "location": "eastus"
    }
  },
  "kubernetes": {
    "version": "1.30.0"
  },
  "node": {
    "maxPods": 110,
    "kubelet": {
      "dnsServiceIP": "10.0.0.10",
      "serverURL": "https://my-cluster-abcd1234.hcp.eastus.azmk8s.io:443",
      "caCertData": "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t..."
    },
    "labels": {
      "node.kubernetes.io/instance-type": "edge-node",
      "environment": "production"
    }
  },
  "components": {
    "containerd": {
      "version": "1.7.1",
      "configPath": "/etc/containerd/config.toml"
    },
    "runc": {
      "version": "1.1.8",
      "path": "/usr/local/sbin/runc"
    },
    "cni": {
      "version": "1.3.0",
      "configDir": "/etc/cni/net.d",
      "binDir": "/opt/cni/bin"
    }
  },
  "systemPaths": {
    "kubeletDir": "/var/lib/kubelet",
    "podManifestPath": "/etc/kubernetes/manifests",
    "kubeConfigPath": "/etc/kubernetes/kubelet/kubeconfig"
  }
}
```

**Complexity Challenges:**
- **Nearly 50 configuration fields** requiring manual population
- **Cluster-specific values** that users must discover and copy manually
- **Version compatibility** requirements between components
- **Path configurations** that vary by OS and installation method

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

### Node Bootstrap Process

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

### Continuous Monitoring & Self-Reconciliation (Optional)

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

Based on the technical constraints of the agent architecture, the following operations are realistically supported:

##### ✅ Feasible Operations

1. **Update/Patch/Upgrade Operations**

   `PUT .../flexNodeConfig/{nodeName}`
   - Node configuration changes (labels, taints, kubelet settings)
   - Kubernetes version upgrades (with controlled kubelet restart)
   - Container runtime updates (containerd/runc)
   - Non-disruptive configuration changes

2. **Registration & Discovery**

   `GET .../flexNodeConfig/{nodeName}` for config & status,
   `GET .../flexNodeConfig` for list
   - Node registration with AKS RP for management visibility
   - **Logical** Machine resource creation (represents the flex-node, not an Azure VM)
   - Operation result confirmation (success/failure of reconciliation attempts)

#### Version-Based Change Detection

The continuous monitoring system uses efficient version-based change detection:

- **Config Versioning**: Each configuration has a Unix timestamp-based version with natural ordering (e.g., `configVersion: "1708123456"`)
- **Polling**: Agent polls every 30 seconds checking for version changes
- **Change Detection**: When version changes (`"1708123456" → "1708127056"`), agent knows config updated
- **Efficiency**: Agent only needs to compare timestamp strings, not entire config content

##### ❌ Not Feasible Operations

1. **Stop/Start Operations**
   - **Problem**: If agent stops the machine manually, it cannot receive start commands
   - **Alternative**: Use reboot operations for restarts, external orchestration for stop/start

2. **Create/Delete Operations**
   - **Problem**: Agent cannot create or destroy its own host machine
   - **Alternative**: Machine provisioning/deprovisioning handled outside of AKS RP integration

## API Design & Implementation Details

### FlexNode Configuration API

The FlexNode configuration API replaces complex manual configuration with AKS RP-generated configurations that include all cluster-specific details.

#### FlexNode Config Resource Creation

**Example 1: Bootstrap-Only Mode (No Continuous Monitoring)**
```http
PUT /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}?api-version=2025-10-02-preview&configUrl=true

Authorization: Bearer {customer-azure-token}
Content-Type: application/json

{
  "properties": {
    "continuousMonitoring": false,
    "kubernetesAuth": {
      "method": "bootstrapToken"
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
```

**Example 2: Continuous Monitoring Mode**
```http
PUT /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}?api-version=2025-10-02-preview&configUrl=true

Authorization: Bearer {customer-azure-token}
Content-Type: application/json

{
  "properties": {
    "continuousMonitoring": true,
    "kubernetesAuth": {
      "method": "bootstrapToken"
    },
    "azureAuth": {
      "method": "servicePrincipal",
      "servicePrincipal": {
        "clientId": "11111111-2222-3333-4444-555555555555",
        "clientSecret": "your-secret-here"
      }
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
```

**Example 3: Service Principal for Both Kubernetes and Azure Auth (AAD Cluster)**
```http
PUT /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}?api-version=2025-10-02-preview&configUrl=true

Authorization: Bearer {customer-azure-token}
Content-Type: application/json

{
  "properties": {
    "continuousMonitoring": true,
    "azureAuth": {
      "method": "servicePrincipal",
      "servicePrincipal": {
        "clientId": "11111111-2222-3333-4444-555555555555",
        "clientSecret": "your-secret-here"
      }
    },
    "kubernetesAuth": {
      "method": "servicePrincipal",
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
    "configUrlExpires": "2024-02-27T10:00:00Z",
    "monitoringEndpoint": "/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}"
  }
}
```

#### Configuration Bundle (Retrieved from Config URL)

**Example: Bootstrap-Only Mode**
```http
GET https://aksconfigs.blob.core.windows.net/configs/{nodeName}-config.json?sv=2022-11-02&...

Response:
{
  "initialConfig": {
    "azure": {
      "subscriptionId": "auto-populated-from-cluster",
      "tenantId": "auto-populated-from-subscription",
      "bootstrapToken": {
        "token": "abcdef.0123456789abcdef"  // Generated by AKS RP
      },
      "targetCluster": {
        "resourceId": "/subscriptions/.../managedClusters/cluster-name",
        "location": "cluster-location"
      }
    },
    // ... rest of config
  }
  // No monitoringEndpoint for bootstrap-only mode
}
```

**Example: Continuous Monitoring Mode**
```http
GET https://aksconfigs.blob.core.windows.net/configs/{nodeName}-config.json?sv=2022-11-02&...

Response:
{
  "initialConfig": {
    "azure": {
      "subscriptionId": "auto-populated-from-cluster",
      "tenantId": "auto-populated-from-subscription",
      "servicePrincipal": {
        "clientId": "11111111-2222-3333-4444-555555555555",
        "clientSecret": "your-secret-here"
      },
      "targetCluster": {
        "resourceId": "/subscriptions/.../managedClusters/cluster-name",
        "location": "cluster-location"
      }
    },
    // ... rest of config
  },
  "monitoringEndpoint": {
    "url": "/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}",
    "azureAuth": "servicePrincipal"
  }
}
```

#### Configuration Updates (Customer-Initiated)
```http
PUT /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}?api-version=2025-10-02-preview

Authorization: Bearer {customer-azure-token}
Content-Type: application/json

{
  "properties": {
    "kubernetes": {
      "version": "1.31.0"
    },
    "containerd": {
      "version": "1.7.2"
    }
  }
}

Response 200 OK:
{
  "configVersion": "1708127056",
  "lastModified": "2024-02-26T11:00:00Z"
}
```

#### Continuous Configuration Monitoring
```http
GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig/{nodeName}?api-version=2025-10-02-preview

Authorization: Bearer {azure-aad-token}

Response:
{
  "name": "edge-node-1",
  "properties": {
    "provisioningState": "Succeeded",
    "configVersion": "1708123456",
    "nodeStatus": "Ready",
    "lastSeen": "2024-02-26T15:30:45Z",
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
        "version": "1.31.0"  // Updated by customer
      },
      "node": {
        "maxPods": 110,
        "kubelet": {
          "dnsServiceIP": "cluster-dns-service-ip",
          "serverURL": "cluster-api-server-url",
          "caCertData": "cluster-ca-cert-base64"
        }
      },
      // ... rest of configuration
      // Note: Authentication details not exposed in monitoring responses
    }
  }
}
```

#### FlexNode Config List Operation (Metadata Only)
```http
GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{cluster}/flexNodeConfig?api-version=2025-10-02-preview

Authorization: Bearer {customer-azure-token}

Response 200 OK:
{
  "value": [
    {
      "name": "edge-node-1",
      "properties": {
        "provisioningState": "Succeeded",
        "nodeStatus": "Ready",
        "lastSeen": "2024-02-26T15:30:45Z",
        "configVersion": "1708123456",
      }
    },
    {
      "name": "edge-node-2",
      "properties": {
        "provisioningState": "Succeeded",
        "nodeStatus": "NotReady",
        "lastSeen": null,
        "configVersion": "1708120000",
      }
    },
    {
      "name": "edge-node-3",
      "properties": {
        "provisioningState": "Succeeded",
        "nodeStatus": "Ready",
        "lastSeen": "2024-02-26T15:29:12Z",
        "configVersion": "1708125000",
      }
    }
  ]
}
```


## Future Improvements

### Rollback Strategy
- **Current Gap**: No mechanism for handling failed configuration updates or rolling back to previous known-good configurations
- **Future Enhancement**: Implement configuration rollback capabilities with version history and automatic failure detection
- **Considerations**:
  - Maintain configuration version history for rollback scenarios
  - Define failure detection criteria (e.g., agent health checks, Kubernetes node status)
  - Provide manual rollback commands for emergency situations

### Scale Limits and Optimization
- **Current Gap**: No analysis of polling frequency impact when managing hundreds or thousands of flex nodes
- **Future Enhancement**: Implement scalable polling strategies and load balancing
- **Considerations**:
  - Evaluate AKS RP load capacity with high node counts (1000+ nodes polling every 30s = 33 RPS)
  - Consider implementing jittered polling intervals to distribute load
  - Explore push-based notifications or webhooks as alternative to polling
  - Add regional load balancing and caching strategies for configuration delivery

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

2. **Manual Local Changes** (drift):
   - Agent detects running config differs from `applied-config.json`
   - Options: **Reconcile** (revert to AKS RP config) or **Alert** (report drift to AKS RP)
   - Behavior configurable based on drift tolerance policy

3. **Agent Restart**:
   - Agent reads `current-config.json` and `config-version.txt`
   - Resumes polling from last known version
   - Ensures no configuration loss during downtime

### Q: How are sensitive credentials handled securely?

**A: Different authentication methods have different security models.**

**Bootstrap Token:**
- ✅ **Secure**: Short-lived (24h), single-use, automatically rotated
- ✅ **Least Privilege**: Limited to node joining only
- ⚠️ **Initial Distribution**: Via config URL (temporary exposure risk)

**Service Principal:**
- ✅ **Customer Managed**: User controls credential lifecycle
- ✅ **Certificate Option**: Can use certs instead of secrets
- ⚠️ **Long-lived**: Requires manual rotation

**Managed Identity:**
- ✅ **Most Secure**: No credential distribution, Azure-managed
- ✅ **Automatic Rotation**: Azure handles credential lifecycle
- ❌ **Azure VMs Only**: Cannot be used on non-Azure hardware

**Future Security Improvements:**
- Replace config URLs with direct API credential exchange
- Support certificate-based authentication for all methods
- Implement automatic credential rotation for all auth types