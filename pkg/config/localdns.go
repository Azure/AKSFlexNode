package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	LocalDNSModeRequired  = "Required"
	LocalDNSModePreferred = "Preferred"
	LocalDNSModeDisabled  = "Disabled"
)

// LocalDNSProfile mirrors the AKS node-pool LocalDNS configuration contract.
type LocalDNSProfile struct {
	Mode             string                      `json:"mode"`
	VnetDNSOverrides map[string]LocalDNSOverride `json:"vnetDNSOverrides,omitempty"`
	KubeDNSOverrides map[string]LocalDNSOverride `json:"kubeDNSOverrides,omitempty"`
}

// LocalDNSOverride configures one CoreDNS server block.
type LocalDNSOverride struct {
	QueryLogging                string `json:"queryLogging,omitempty"`
	Protocol                    string `json:"protocol,omitempty"`
	ForwardDestination          string `json:"forwardDestination,omitempty"`
	ForwardPolicy               string `json:"forwardPolicy,omitempty"`
	MaxConcurrent               int    `json:"maxConcurrent,omitempty"`
	CacheDurationInSeconds      int    `json:"cacheDurationInSeconds,omitempty"`
	ServeStaleDurationInSeconds int    `json:"serveStaleDurationInSeconds,omitempty"`
	ServeStale                  string `json:"serveStale,omitempty"`
}

func (p *LocalDNSProfile) validate() error {
	if p == nil {
		return nil
	}
	if p.Mode != LocalDNSModeRequired && p.Mode != LocalDNSModePreferred && p.Mode != LocalDNSModeDisabled {
		return fmt.Errorf("mode must be Required, Preferred, or Disabled")
	}
	var errs []error
	for class, overrides := range map[string]map[string]LocalDNSOverride{
		"vnetDNSOverrides": p.VnetDNSOverrides,
		"kubeDNSOverrides": p.KubeDNSOverrides,
	} {
		for zone, override := range overrides {
			if strings.TrimSpace(zone) == "" || strings.ContainsAny(zone, "{} \t\r\n") {
				errs = append(errs, fmt.Errorf("%s zone %q is invalid", class, zone))
			}
			if err := override.validate(); err != nil {
				errs = append(errs, fmt.Errorf("%s[%q]: %w", class, zone, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (o LocalDNSOverride) validate() error {
	var errs []error
	if o.QueryLogging != "" && o.QueryLogging != "Error" && o.QueryLogging != "Log" {
		errs = append(errs, fmt.Errorf("queryLogging must be Error or Log"))
	}
	if o.Protocol != "" && o.Protocol != "PreferUDP" && o.Protocol != "ForceTCP" {
		errs = append(errs, fmt.Errorf("protocol must be PreferUDP or ForceTCP"))
	}
	if o.ForwardDestination != "" && o.ForwardDestination != "VnetDNS" && o.ForwardDestination != "ClusterCoreDNS" {
		errs = append(errs, fmt.Errorf("forwardDestination must be VnetDNS or ClusterCoreDNS"))
	}
	if o.ForwardPolicy != "" && o.ForwardPolicy != "Sequential" && o.ForwardPolicy != "RoundRobin" && o.ForwardPolicy != "Random" {
		errs = append(errs, fmt.Errorf("forwardPolicy must be Sequential, RoundRobin, or Random"))
	}
	if o.ServeStale != "" && o.ServeStale != "Disable" && o.ServeStale != "Verify" && o.ServeStale != "Immediate" {
		errs = append(errs, fmt.Errorf("serveStale must be Disable, Verify, or Immediate"))
	}
	for name, value := range map[string]int{
		"maxConcurrent":               o.MaxConcurrent,
		"cacheDurationInSeconds":      o.CacheDurationInSeconds,
		"serveStaleDurationInSeconds": o.ServeStaleDurationInSeconds,
	} {
		if value < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative", name))
		}
	}
	return errors.Join(errs...)
}

// Enabled reports whether the profile requires LocalDNS installation.
func (p *LocalDNSProfile) Enabled() bool {
	return p != nil && p.Mode == LocalDNSModeRequired
}

// CorefileTemplate renders AKS LocalDNS policy into an Unbounded Corefile template.
func (p *LocalDNSProfile) CorefileTemplate() (string, error) {
	if p == nil {
		return "", nil
	}
	if err := p.validate(); err != nil {
		return "", err
	}
	if !p.Enabled() {
		return "", nil
	}

	vnet := p.VnetDNSOverrides
	if len(vnet) == 0 {
		vnet = map[string]LocalDNSOverride{".": {ForwardDestination: "VnetDNS"}}
	}
	kube := p.KubeDNSOverrides
	if len(kube) == 0 {
		kube = map[string]LocalDNSOverride{".": {ForwardDestination: "ClusterCoreDNS"}}
	}

	var out strings.Builder
	out.WriteString("health-check.localdns.local:53 {\n    bind {{ .NodeListenerIP }} {{ .ClusterListenerIP }}\n    whoami\n}\n\n")
	renderLocalDNSBlocks(&out, vnet, "{{ .NodeListenerIP }}", "VnetDNS")
	renderLocalDNSBlocks(&out, kube, "{{ .ClusterListenerIP }}", "ClusterCoreDNS")
	return out.String(), nil
}

func renderLocalDNSBlocks(out *strings.Builder, overrides map[string]LocalDNSOverride, listener, defaultDestination string) {
	zones := make([]string, 0, len(overrides))
	for zone := range overrides {
		zones = append(zones, zone)
	}
	sort.Strings(zones)
	for _, zone := range zones {
		o := withLocalDNSDefaults(overrides[zone], defaultDestination)
		fmt.Fprintf(out, "%s:53 {\n", zone)
		if o.QueryLogging == "Log" {
			out.WriteString("    log\n")
		} else {
			out.WriteString("    errors\n")
		}
		fmt.Fprintf(out, "    bind %s\n", listener)
		upstream := "{{ .NodeUpstreamIPsJoined }}"
		if o.ForwardDestination == "ClusterCoreDNS" {
			upstream = "{{ .ClusterDNSServiceIP }}"
		}
		fmt.Fprintf(out, "    forward . %s {\n", upstream)
		if o.Protocol == "ForceTCP" {
			out.WriteString("        force_tcp\n")
		}
		fmt.Fprintf(out, "        policy %s\n        max_concurrent %d\n    }\n", localDNSForwardPolicy(o.ForwardPolicy), o.MaxConcurrent)
		fmt.Fprintf(out, "    ready %s:8181\n", listener)
		fmt.Fprintf(out, "    cache %d {\n        success 9984\n        denial 9984\n", o.CacheDurationInSeconds)
		if o.ServeStale != "Disable" {
			fmt.Fprintf(out, "        serve_stale %ds %s\n", o.ServeStaleDurationInSeconds, strings.ToLower(o.ServeStale))
		}
		out.WriteString("        servfail 0\n    }\n    loop\n    prometheus {{ .MetricsAddress }}\n}\n\n")
	}
}

func withLocalDNSDefaults(o LocalDNSOverride, destination string) LocalDNSOverride {
	if o.QueryLogging == "" {
		o.QueryLogging = "Error"
	}
	if o.Protocol == "" {
		o.Protocol = "ForceTCP"
	}
	if o.ForwardDestination == "" {
		o.ForwardDestination = destination
	}
	if o.ForwardPolicy == "" {
		o.ForwardPolicy = "Sequential"
	}
	if o.MaxConcurrent == 0 {
		o.MaxConcurrent = 1000
	}
	if o.CacheDurationInSeconds == 0 {
		o.CacheDurationInSeconds = 3600
	}
	if o.ServeStaleDurationInSeconds == 0 {
		o.ServeStaleDurationInSeconds = 3600
	}
	if o.ServeStale == "" {
		o.ServeStale = "Immediate"
	}
	return o
}

func localDNSForwardPolicy(policy string) string {
	switch policy {
	case "RoundRobin":
		return "round_robin"
	case "Random":
		return "random"
	default:
		return "sequential"
	}
}
