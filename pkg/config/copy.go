package config

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// DeepCopy returns a copy of the config that does not share mutable sub-objects (maps/pointers)
// with the original.
func (cfg *Config) DeepCopy() *Config {
	if cfg == nil {
		return nil
	}

	out := *cfg

	// Copy pointer sub-structs under Azure.
	if cfg.Azure.ServicePrincipal != nil {
		sp := *cfg.Azure.ServicePrincipal
		out.Azure.ServicePrincipal = &sp
	}
	if cfg.Azure.ManagedIdentity != nil {
		mi := *cfg.Azure.ManagedIdentity
		out.Azure.ManagedIdentity = &mi
	}
	if cfg.Azure.BootstrapToken != nil {
		bt := *cfg.Azure.BootstrapToken
		out.Azure.BootstrapToken = &bt
	}
	if cfg.Azure.TargetCluster != nil {
		tc := *cfg.Azure.TargetCluster
		out.Azure.TargetCluster = &tc
	}
	if cfg.Azure.Arc != nil {
		arc := *cfg.Azure.Arc
		arc.Tags = cloneStringMap(cfg.Azure.Arc.Tags)
		out.Azure.Arc = &arc
	}

	// Copy node-level maps.
	out.Node.Labels = cloneStringMap(cfg.Node.Labels)
	out.Node.Kubelet.KubeReserved = cloneStringMap(cfg.Node.Kubelet.KubeReserved)
	out.Node.Kubelet.EvictionHard = cloneStringMap(cfg.Node.Kubelet.EvictionHard)

	return &out
}
