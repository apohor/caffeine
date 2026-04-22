// Package config holds runtime configuration for caffeine.
package config

// Config captures all runtime knobs. Loaded from flags + env in cmd/caffeine.
type Config struct {
	// Addr is the HTTP listen address, e.g. ":8080".
	Addr string
	// MachineURL is the base URL of the Meticulous machine REST API.
	MachineURL string
}
