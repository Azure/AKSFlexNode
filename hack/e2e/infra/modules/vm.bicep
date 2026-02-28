// =============================================================================
// modules/vm.bicep - Reusable Ubuntu flex-node VM module
//
// Creates a public IP, NIC, and Ubuntu 22.04 VM in the given subnet.
// =============================================================================

@description('Azure region for all resources.')
param location string

@description('VM name (also used as prefix for NIC and public IP names).')
param vmName string

@description('VM size.')
param vmSize string

@description('Admin username.')
param adminUsername string

@description('SSH public key.')
@secure()
param sshPublicKey string

@description('Subnet resource ID to attach the NIC to.')
param subnetId string

@description('Whether to assign a system-assigned managed identity to the VM.')
param assignManagedIdentity bool = false

@description('Tags applied to all resources in this module.')
param tags object = {}

// ---------------------------------------------------------------------------
// Public IP
// ---------------------------------------------------------------------------
resource pip 'Microsoft.Network/publicIPAddresses@2023-11-01' = {
  name: '${vmName}-pip'
  location: location
  tags: tags
  sku: { name: 'Standard' }
  properties: {
    publicIPAllocationMethod: 'Static'
  }
}

// ---------------------------------------------------------------------------
// NIC
// ---------------------------------------------------------------------------
resource nic 'Microsoft.Network/networkInterfaces@2023-11-01' = {
  name: '${vmName}-nic'
  location: location
  tags: tags
  properties: {
    ipConfigurations: [
      {
        name: 'ipconfig1'
        properties: {
          subnet: {
            id: subnetId
          }
          publicIPAddress: {
            id: pip.id
          }
          privateIPAllocationMethod: 'Dynamic'
        }
      }
    ]
  }
}

// ---------------------------------------------------------------------------
// VM
// ---------------------------------------------------------------------------
resource vm 'Microsoft.Compute/virtualMachines@2024-03-01' = {
  name: vmName
  location: location
  tags: tags
  identity: assignManagedIdentity ? {
    type: 'SystemAssigned'
  } : {
    type: 'None'
  }
  properties: {
    hardwareProfile: { vmSize: vmSize }
    osProfile: {
      computerName: vmName
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
      networkInterfaces: [ { id: nic.id } ]
    }
  }
}

// ---------------------------------------------------------------------------
// Outputs
// ---------------------------------------------------------------------------
output vmName string = vm.name
output publicIpAddress string = pip.properties.ipAddress
output principalId string = assignManagedIdentity ? vm.identity.principalId : ''
