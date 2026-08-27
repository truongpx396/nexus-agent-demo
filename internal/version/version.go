// Package version reports the build identity shared by every binary
// (nexusd, nexusctl, signerd) so they can assert they were built from the
// same commit before wiring up the control-plane <-> data-plane contract.
package version

// Version is overridden at build time via -ldflags "-X ... .Version=...".
var Version = "dev"

// GitCommit is overridden at build time via -ldflags.
var GitCommit = "unknown"
