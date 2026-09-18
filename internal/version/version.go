// Package version carries build metadata injected by -ldflags.
package version

import (
	"runtime"
	"strings"
)

// Values overridden at build time (see the Makefile).
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// String renders the one-line banner.
func String() string {
	return strings.Join([]string{
		"deal-hunter " + Version,
		"commit=" + Commit,
		"built=" + BuildDate,
		runtime.Version() + "/" + runtime.GOOS + "-" + runtime.GOARCH,
	}, " · ")
}
