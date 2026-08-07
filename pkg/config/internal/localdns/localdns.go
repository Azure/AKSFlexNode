package localdns

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

const (
	LocalDNSModeRequired  = "Required"
	LocalDNSModePreferred = "Preferred"
	LocalDNSModeDisabled  = "Disabled"
)

// LocalDNSProfile mirrors the AKS node-pool LocalDNS configuration contract.
// Keep rendering behavior aligned with AgentBaker:
// https://github.com/Azure/AgentBaker/blob/62a783b7a967352bb4726e636d22930c9973f4f7/aks-node-controller/parser/templates/localdns.toml.gtpl
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

// Validate verifies the AKS LocalDNS profile values accepted by the renderer.
func (p *LocalDNSProfile) Validate() error {
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

//go:embed assets/localdns.corefile.tmpl
var aksLocalDNSCorefileTemplate string

type localDNSCorefileData struct {
	NodeListener    string
	ClusterListener string
	MetricsAddress  string
	Blocks          []localDNSCorefileBlock
}

type localDNSCorefileBlock struct {
	Zone               string
	IsRootDomain       bool
	Listener           string
	Upstream           string
	NSID               string
	LogQueries         bool
	ForceTCP           bool
	ForwardPolicy      string
	MaxConcurrent      int
	CacheDuration      int
	ServeStale         bool
	ServeStaleDuration int
	ServeStalePolicy   string
}

// CorefileTemplate renders AKS LocalDNS policy into an Unbounded Corefile template.
func (p *LocalDNSProfile) CorefileTemplate() (string, error) {
	if p == nil {
		return "", nil
	}
	if err := p.Validate(); err != nil {
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

	// Rendering happens in two stages. This pass translates the AKS profile into
	// a Corefile template; Unbounded later resolves these literal placeholders
	// from machine-specific listener, upstream, cluster DNS, and metrics values.
	data := localDNSCorefileData{
		NodeListener:    "{{ .NodeListenerIP }}",
		ClusterListener: "{{ .ClusterListenerIP }}",
		MetricsAddress:  "{{ .MetricsAddress }}",
		Blocks:          append(localDNSCorefileBlocks(vnet, "{{ .NodeListenerIP }}", "VnetDNS"), localDNSCorefileBlocks(kube, "{{ .ClusterListenerIP }}", "ClusterCoreDNS")...),
	}
	tmpl, err := template.New("aks-localdns-corefile").Parse(aksLocalDNSCorefileTemplate)
	if err != nil {
		return "", fmt.Errorf("parse AKS LocalDNS Corefile template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render AKS LocalDNS Corefile template: %w", err)
	}
	return out.String(), nil
}

func localDNSCorefileBlocks(overrides map[string]LocalDNSOverride, listener, defaultDestination string) []localDNSCorefileBlock {
	zones := make([]string, 0, len(overrides))
	for zone := range overrides {
		zones = append(zones, zone)
	}
	sort.Strings(zones)

	blocks := make([]localDNSCorefileBlock, 0, len(zones))
	for _, zone := range zones {
		o := withLocalDNSDefaults(overrides[zone], defaultDestination)
		isRootDomain := zone == "."
		forwardToClusterDNS := o.ForwardDestination == "ClusterCoreDNS" || strings.HasSuffix(zone, "cluster.local")
		// AgentBaker always sends the VnetDNS root zone to the node's DNS path.
		if defaultDestination == "VnetDNS" && isRootDomain {
			forwardToClusterDNS = false
		}

		upstream := "{{ .NodeUpstreamIPsJoined }}"
		if forwardToClusterDNS {
			upstream = "{{ .ClusterDNSServiceIP }}"
		}
		nsid := "localdns"
		if defaultDestination == "ClusterCoreDNS" {
			nsid = "localdns-pod"
		}
		blocks = append(blocks, localDNSCorefileBlock{
			Zone:               zone,
			IsRootDomain:       isRootDomain,
			Listener:           listener,
			Upstream:           upstream,
			NSID:               nsid,
			LogQueries:         o.QueryLogging == "Log",
			ForceTCP:           o.Protocol == "ForceTCP",
			ForwardPolicy:      localDNSForwardPolicy(o.ForwardPolicy),
			MaxConcurrent:      o.MaxConcurrent,
			CacheDuration:      o.CacheDurationInSeconds,
			ServeStale:         o.ServeStale != "Disable",
			ServeStaleDuration: o.ServeStaleDurationInSeconds,
			ServeStalePolicy:   strings.ToLower(o.ServeStale),
		})
	}
	return blocks
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
