package config

import "maps"

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
		arc.Tags = maps.Clone(cfg.Azure.Arc.Tags)
		out.Azure.Arc = &arc
	}

	// Copy node-level maps.
	out.Node.Labels = maps.Clone(cfg.Node.Labels)
	if cfg.Node.Taints != nil {
		out.Node.Taints = append([]string(nil), cfg.Node.Taints...)
	}

	return &out
}
