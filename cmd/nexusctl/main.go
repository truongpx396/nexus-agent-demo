// Command nexusctl is the CLI surface adapter — the second surface that
// proves Principle I (one loop, many surfaces). Its real logic lives in
// internal/surfaces/cli (README task 7.15), the same split cmd/nexusd has
// over internal/surfaces/rest; this file is a thin wrapper over Main so the
// logic itself is testable without a subprocess.
package main

import (
	"fmt"
	"os"

	"github.com/truongpx396/nexus-agent-demo/internal/dotenv"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/cli"
)

func main() {
	// See cmd/nexusd/main.go's identical call for why this comes first —
	// cli.Main reads NEXUS_HTTP_ADDR/NEXUS_TOKEN via envOr before anything
	// else runs.
	if err := dotenv.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "nexusctl: %v\n", err)
	}
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
