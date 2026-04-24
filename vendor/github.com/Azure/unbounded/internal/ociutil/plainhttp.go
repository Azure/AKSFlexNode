// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package ociutil provides shared helpers for OCI registry operations.
package ociutil

import (
	"net"

	"oras.land/oras-go/v2/registry/remote"
)

// ConfigurePlainHTTP sets PlainHTTP on the repository when the registry host
// is a loopback address (localhost, 127.0.0.1, [::1]) or a private IP
// (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16). These registries almost
// never serve TLS and defaulting to HTTPS causes "http: server gave HTTP
// response to HTTPS client" errors.
func ConfigurePlainHTTP(repo *remote.Repository) {
	host := repo.Reference.Host()

	// Strip the port if present so we can parse the bare IP/hostname.
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}

	if hostname == "localhost" {
		repo.PlainHTTP = true
		return
	}

	ip := net.ParseIP(hostname)
	if ip == nil {
		return
	}

	if ip.IsLoopback() || ip.IsPrivate() {
		repo.PlainHTTP = true
	}
}
