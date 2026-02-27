package cni

import "github.com/Azure/AKSFlexNode/pkg/config"

func (x *DownloadCNIBinaries) Defaulting() {
	if x.HasSpec() {
		x.GetSpec().Defaulting()
	}
}

func (x *DownloadCNIBinariesSpec) Defaulting() {
	if !x.HasCniPluginsVersion() {
		x.SetCniPluginsVersion(config.DefaultCNIPluginsVersion)
	}
}

func (x *ConfigureCNI) Defaulting() {
	if x.HasSpec() {
		x.GetSpec().Defaulting()
	}
}

func (x *ConfigureCNISpec) Defaulting() {
	if !x.HasCniSpecVersion() {
		x.SetCniSpecVersion(config.DefaultCNISpecVersion)
	}
}
