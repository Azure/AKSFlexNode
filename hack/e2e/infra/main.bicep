// =============================================================================
// AKS Flex Node E2E Infrastructure
//
// Deploys:
//   - AKS cluster (1-node control plane)
//   - VM with system-assigned managed identity  (MSI auth mode)
//   - VM without managed identity               (bootstrap token auth mode)
//
// Both VMs run Ubuntu 22.04 LTS, have public IPs, and allow SSH ingress.
// =============================================================================

@description('Azure region for all resources.')
param location string = resourceGroup().location

@description('Unique suffix for resource names (e.g. epoch timestamp).')
param nameSuffix string = uniqueString(resourceGroup().id)

@description('AKS node VM size.')
param aksNodeVmSize string = 'Standard_B2s'

@description('Flex node VM size.')
param vmSize string = 'Standard_B2ms'

@description('Admin username for VMs.')
param adminUsername string = 'azureuser'

@description('SSH public key for VM access.')
@secure()
param sshPublicKey string

@description('Tags applied to every resource.')
param tags object = {}

// ---------------------------------------------------------------------------
// Variables
// ---------------------------------------------------------------------------
var clusterName   = 'aks-e2e-${nameSuffix}'
var msiVmName     = 'vm-e2e-msi-${nameSuffix}'
var tokenVmName   = 'vm-e2e-token-${nameSuffix}'
var kubeadmVmName = 'vm-e2e-kubeadm-${nameSuffix}'
var vnetName      = 'vnet-e2e-${nameSuffix}'
var nsgName       = 'nsg-e2e-${nameSuffix}'

var subnetAksName = 'snet-aks'
var subnetVmName  = 'snet-vm'

// ---------------------------------------------------------------------------
// Network Security Group
// ---------------------------------------------------------------------------
resource nsg 'Microsoft.Network/networkSecurityGroups@2023-11-01' = {
  name: nsgName
  location: location
  tags: tags
  properties: {
    securityRules: [
      {
        name: 'AllowSSH'
        properties: {
          priority: 1000
          direction: 'Inbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourceAddressPrefix: '*'
          sourcePortRange: '*'
          destinationAddressPrefix: '*'
          destinationPortRange: '22'
        }
      }
    ]
  }
}

// ---------------------------------------------------------------------------
// Virtual Network
// ---------------------------------------------------------------------------
resource vnet 'Microsoft.Network/virtualNetworks@2023-11-01' = {
  name: vnetName
  location: location
  tags: tags
  properties: {
    addressSpace: {
      addressPrefixes: [ '10.224.0.0/12' ]
    }
    subnets: [
      {
        name: subnetAksName
        properties: {
          addressPrefix: '10.224.0.0/16'
        }
      }
      {
        name: subnetVmName
        properties: {
          addressPrefix: '10.225.0.0/24'
          networkSecurityGroup: {
            id: nsg.id
          }
        }
      }
    ]
  }
}

// ---------------------------------------------------------------------------
// AKS Cluster
// ---------------------------------------------------------------------------
resource aksCluster 'Microsoft.ContainerService/managedClusters@2024-01-01' = {
  name: clusterName
  location: location
  tags: tags
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    dnsPrefix: clusterName
    enableRBAC: true
    aadProfile: {
      managed: true
      enableAzureRBAC: true
    }
    networkProfile: {
      networkPlugin: 'azure'
      serviceCidr: '10.0.0.0/16'
      dnsServiceIP: '10.0.0.10'
    }
    agentPoolProfiles: [
      {
        name: 'system'
        count: 1
        vmSize: aksNodeVmSize
        mode: 'System'
        osType: 'Linux'
        vnetSubnetID: vnet.properties.subnets[0].id
      }
    ]
  }
}

// ---------------------------------------------------------------------------
// Public IPs for VMs
// ---------------------------------------------------------------------------
resource pipMsi 'Microsoft.Network/publicIPAddresses@2023-11-01' = {
  name: '${msiVmName}-pip'
  location: location
  tags: tags
  sku: { name: 'Standard' }
  properties: {
    publicIPAllocationMethod: 'Static'
  }
}

resource pipToken 'Microsoft.Network/publicIPAddresses@2023-11-01' = {
  name: '${tokenVmName}-pip'
  location: location
  tags: tags
  sku: { name: 'Standard' }
  properties: {
    publicIPAllocationMethod: 'Static'
  }
}

resource pipKubeadm 'Microsoft.Network/publicIPAddresses@2023-11-01' = {
  name: '${kubeadmVmName}-pip'
  location: location
  tags: tags
  sku: { name: 'Standard' }
  properties: {
    publicIPAllocationMethod: 'Static'
  }
}

// ---------------------------------------------------------------------------
// NICs
// ---------------------------------------------------------------------------
resource nicMsi 'Microsoft.Network/networkInterfaces@2023-11-01' = {
  name: '${msiVmName}-nic'
  location: location
  tags: tags
  properties: {
    ipConfigurations: [
      {
        name: 'ipconfig1'
        properties: {
          subnet: {
            id: vnet.properties.subnets[1].id
          }
          publicIPAddress: {
            id: pipMsi.id
          }
          privateIPAllocationMethod: 'Dynamic'
        }
      }
    ]
  }
}

resource nicToken 'Microsoft.Network/networkInterfaces@2023-11-01' = {
  name: '${tokenVmName}-nic'
  location: location
  tags: tags
  properties: {
    ipConfigurations: [
      {
        name: 'ipconfig1'
        properties: {
          subnet: {
            id: vnet.properties.subnets[1].id
          }
          publicIPAddress: {
            id: pipToken.id
          }
          privateIPAllocationMethod: 'Dynamic'
        }
      }
    ]
  }
}

resource nicKubeadm 'Microsoft.Network/networkInterfaces@2023-11-01' = {
  name: '${kubeadmVmName}-nic'
  location: location
  tags: tags
  properties: {
    ipConfigurations: [
      {
        name: 'ipconfig1'
        properties: {
          subnet: {
            id: vnet.properties.subnets[1].id
          }
          publicIPAddress: {
            id: pipKubeadm.id
          }
          privateIPAllocationMethod: 'Dynamic'
        }
      }
    ]
  }
}

// ---------------------------------------------------------------------------
// VM: MSI (system-assigned managed identity)
// ---------------------------------------------------------------------------
resource vmMsi 'Microsoft.Compute/virtualMachines@2024-03-01' = {
  name: msiVmName
  location: location
  tags: tags
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    hardwareProfile: { vmSize: vmSize }
    osProfile: {
      computerName: msiVmName
      adminUsername: adminUsername
      linuxConfiguration: {
        disablePasswordAuthentication: true
        ssh: {
          publicKeys: [
            {
              path: '/home/${adminUsername}/.ssh/authorized_keys'
              keyData: sshPublicKey
            }
          ]
        }
      }
    }
    storageProfile: {
      imageReference: {
        publisher: 'Canonical'
        offer: '0001-com-ubuntu-server-jammy'
        sku: '22_04-lts-gen2'
        version: 'latest'
      }
      osDisk: {
        createOption: 'FromImage'
        managedDisk: { storageAccountType: 'StandardSSD_LRS' }
      }
    }
    networkProfile: {
      networkInterfaces: [ { id: nicMsi.id } ]
    }
  }
}

// ---------------------------------------------------------------------------
// VM: Token (no managed identity)
// ---------------------------------------------------------------------------
resource vmToken 'Microsoft.Compute/virtualMachines@2024-03-01' = {
  name: tokenVmName
  location: location
  tags: tags
  properties: {
    hardwareProfile: { vmSize: vmSize }
    osProfile: {
      computerName: tokenVmName
      adminUsername: adminUsername
      linuxConfiguration: {
        disablePasswordAuthentication: true
        ssh: {
          publicKeys: [
            {
              path: '/home/${adminUsername}/.ssh/authorized_keys'
              keyData: sshPublicKey
            }
          ]
        }
      }
    }
    storageProfile: {
      imageReference: {
        publisher: 'Canonical'
        offer: '0001-com-ubuntu-server-jammy'
        sku: '22_04-lts-gen2'
        version: 'latest'
      }
      osDisk: {
        createOption: 'FromImage'
        managedDisk: { storageAccountType: 'StandardSSD_LRS' }
      }
    }
    networkProfile: {
      networkInterfaces: [ { id: nicToken.id } ]
    }
  }
}

// ---------------------------------------------------------------------------
// VM: Kubeadm (no managed identity - uses apply -f with kubeadm join flow)
// ---------------------------------------------------------------------------
resource vmKubeadm 'Microsoft.Compute/virtualMachines@2024-03-01' = {
  name: kubeadmVmName
  location: location
  tags: tags
  properties: {
    hardwareProfile: { vmSize: vmSize }
    osProfile: {
      computerName: kubeadmVmName
      adminUsername: adminUsername
      linuxConfiguration: {
        disablePasswordAuthentication: true
        ssh: {
          publicKeys: [
            {
              path: '/home/${adminUsername}/.ssh/authorized_keys'
              keyData: sshPublicKey
            }
          ]
        }
      }
    }
    storageProfile: {
      imageReference: {
        publisher: 'Canonical'
        offer: '0001-com-ubuntu-server-jammy'
        sku: '22_04-lts-gen2'
        version: 'latest'
      }
      osDisk: {
        createOption: 'FromImage'
        managedDisk: { storageAccountType: 'StandardSSD_LRS' }
      }
    }
    networkProfile: {
      networkInterfaces: [ { id: nicKubeadm.id } ]
    }
  }
}

// ---------------------------------------------------------------------------
// Role assignments: grant MSI VM permissions on the AKS cluster
// ---------------------------------------------------------------------------
// Azure Kubernetes Service Cluster Admin Role
resource roleClusterAdmin 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(aksCluster.id, vmMsi.id, 'aks-cluster-admin')
  scope: aksCluster
  properties: {
    principalId: vmMsi.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '0ab0b1a8-8aac-4efd-b8c2-3ee1fb270be8')
  }
}

// Azure Kubernetes Service RBAC Cluster Admin
resource roleRbacAdmin 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(aksCluster.id, vmMsi.id, 'aks-rbac-cluster-admin')
  scope: aksCluster
  properties: {
    principalId: vmMsi.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b')
  }
}

// ---------------------------------------------------------------------------
// Outputs
// ---------------------------------------------------------------------------
output clusterName string = aksCluster.name
output clusterId string = aksCluster.id
output clusterFqdn string = aksCluster.properties.fqdn

output msiVmName string = vmMsi.name
output msiVmIp string = pipMsi.properties.ipAddress
output msiVmPrincipalId string = vmMsi.identity.principalId

output tokenVmName string = vmToken.name
output tokenVmIp string = pipToken.properties.ipAddress

output kubeadmVmName string = vmKubeadm.name
output kubeadmVmIp string = pipKubeadm.properties.ipAddress

output adminUsername string = adminUsername
