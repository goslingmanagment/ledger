// Package version reports the build the binary was made from.
package version

import "os"

// Build is set at link time: -ldflags "-X github.com/goslingmanagment/ledger/internal/version.Build=<sha>".
var Build = "local"

// Get returns the build; LEDGER_BUILD overrides an unset link-time value (local runs).
func Get() string {
	if Build == "local" {
		if v := os.Getenv("LEDGER_BUILD"); v != "" {
			return v
		}
	}
	return Build
}
