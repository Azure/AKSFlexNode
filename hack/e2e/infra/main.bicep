// =============================================================================
// AKS Flex Node E2E Infrastructure
//
// Deploys:
//   - AKS cluster (1-node control plane)
//   - VM with system-assigned managed identity  (MSI auth mode)
//   - VM without managed identity               (bootstrap token auth mode)
//   - VM without managed identity               (kubeadm apply -f auth mode)
//
// All flex-node VMs run Ubuntu 22.04 LTS, have public IPs, and allow SSH
// ingress.  VM creation is delegated to the reusable modules/vm.bicep module.
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
// Flex-node VMs (via reusable module)
// ---------------------------------------------------------------------------
module vmMsi 'modules/vm.bicep' = {
  name: 'deploy-vm-msi'
  params: {
    location: location
    vmName: msiVmName
    vmSize: vmSize
    adminUsername: adminUsername
    sshPublicKey: sshPublicKey
    subnetId: vnet.properties.subnets[1].id
    assignManagedIdentity: true
    tags: tags
  }
}

module vmToken 'modules/vm.bicep' = {
  name: 'deploy-vm-token'
  params: {
    location: location
    vmName: tokenVmName
    vmSize: vmSize
    adminUsername: adminUsername
    sshPublicKey: sshPublicKey
    subnetId: vnet.properties.subnets[1].id
    assignManagedIdentity: false
    tags: tags
  }
}

module vmKubeadm 'modules/vm.bicep' = {
  name: 'deploy-vm-kubeadm'
  params: {
    location: location
    vmName: kubeadmVmName
    vmSize: vmSize
    adminUsername: adminUsername
    sshPublicKey: sshPublicKey
    subnetId: vnet.properties.subnets[1].id
    assignManagedIdentity: false
    tags: tags
  }
}

// ---------------------------------------------------------------------------
// Role assignments: grant MSI VM permissions on the AKS cluster
// ---------------------------------------------------------------------------
// Azure Kubernetes Service Cluster Admin Role
resource roleClusterAdmin 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(aksCluster.id, vmMsi.outputs.principalId, 'aks-cluster-admin')
  scope: aksCluster
  properties: {
    principalId: vmMsi.outputs.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '0ab0b1a8-8aac-4efd-b8c2-3ee1fb270be8')
  }
}

// Azure Kubernetes Service RBAC Cluster Admin
resource roleRbacAdmin 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(aksCluster.id, vmMsi.outputs.principalId, 'aks-rbac-cluster-admin')
  scope: aksCluster
  properties: {
    principalId: vmMsi.outputs.principalId
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

output msiVmName string = vmMsi.outputs.vmName
output msiVmIp string = vmMsi.outputs.publicIpAddress
output msiVmPrincipalId string = vmMsi.outputs.principalId

output tokenVmName string = vmToken.outputs.vmName
output tokenVmIp string = vmToken.outputs.publicIpAddress

output kubeadmVmName string = vmKubeadm.outputs.vmName
output kubeadmVmIp string = vmKubeadm.outputs.publicIpAddress

output adminUsername string = adminUsername

