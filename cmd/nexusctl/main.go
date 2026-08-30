// Command nexusctl is the CLI surface adapter — the second surface that
// proves Principle I (one loop, many surfaces). Its real logic lives in
// internal/surfaces/cli (README task 7.15), the same split cmd/nexusd has
// over internal/surfaces/rest; this file is a thin wrapper over Main so the
// logic itself is testable without a subprocess.
package main

import (
	"os"

	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
