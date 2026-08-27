// Command nexusctl is the CLI surface adapter — the second surface that
// proves Principle I (one loop, many surfaces): it must translate I/O only
// and add zero control flow of its own. It lands in Phase 7.
package main

import (
	"fmt"
	"os"

	"github.com/truongpx396/nexus-agent-demo/internal/version"
)

func main() {
	if _, err := fmt.Fprintf(os.Stdout, "nexusctl %s (%s)\n", version.Version, version.GitCommit); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprintln(os.Stdout, "scaffold only — surface adapter lands in Phase 7 (see README.md)"); err != nil {
		os.Exit(1)
	}
}
