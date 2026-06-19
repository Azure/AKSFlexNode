// =============================================================================
// modules/vm.bicep - Reusable flex-node VM module
//
// Creates a public IP, NIC, and Linux VM in the given subnet. The marketplace
// image defaults to Ubuntu 24.04 LTS (Noble) but can be overridden. A
// generalized VHD URI can also be imported as a managed image and used as the
// VM source image.
// =============================================================================

@description('Azure region for all resources.')
param location string

@description('VM name (also used as prefix for NIC and public IP names).')
param vmName string

@description('Guest OS hostname. Defaults to vmName.')
param computerName string = vmName

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

@allowed([
  'marketplace'
  'vhd'
])
@description('Image source type. Use marketplace for imageReference fields, or vhd to create a managed image from imageVhdUri.')
param imageSourceType string = 'marketplace'

@description('Marketplace image publisher.')
param imagePublisher string = 'Canonical'

@description('Marketplace image offer.')
param imageOffer string = 'ubuntu-24_04-lts'

@description('Marketplace image SKU.')
param imageSku string = 'server'

@description('Marketplace image version.')
param imageVersion string = 'latest'

@secure()
@description('Generalized Linux VHD URI used when imageSourceType is vhd. The URI must be readable by Azure Compute, for example via SAS.')
param imageVhdUri string = ''

@allowed([
  'V1'
  'V2'
])
@description('Hyper-V generation for a managed image created from imageVhdUri.')
param imageHyperVGeneration string = 'V2'

@description('Tags applied to all resources in this module.')
param tags object = {}

var useVhdImage = imageSourceType == 'vhd'

resource vhdImage 'Microsoft.Compute/images@2024-03-01' = if (useVhdImage) {
  name: '${vmName}-image'
  location: location
  tags: tags
  properties: {
    hyperVGeneration: imageHyperVGeneration
    storageProfile: {
      osDisk: {
        osType: 'Linux'
        osState: 'Generalized'
        blobUri: imageVhdUri
        storageAccountType: 'StandardSSD_LRS'
      }
    }
  }
}

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
      computerName: computerName
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
      imageReference: useVhdImage ? {
        id: vhdImage.id
      } : {
        publisher: imagePublisher
        offer: imageOffer
        sku: imageSku
        version: imageVersion
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
output privateIpAddress string = nic.properties.ipConfigurations[0].properties.privateIPAddress
output principalId string = assignManagedIdentity ? vm.identity.principalId : ''
