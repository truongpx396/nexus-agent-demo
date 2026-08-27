// Command nexusd is the single-binary data+control plane: the kernel loop,
// the harness, and the REST surface, all in one process (see README.md §4).
// It is a scaffold today — the kernel loop lands in Phase 2.
package main

import (
	"fmt"
	"os"

	"github.com/truongpx396/nexus-agent-demo/internal/version"
)

func main() {
	if _, err := fmt.Fprintf(os.Stdout, "nexusd %s (%s)\n", version.Version, version.GitCommit); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprintln(os.Stdout, "scaffold only — kernel loop and REST surface land in Phase 2 (see README.md)"); err != nil {
		os.Exit(1)
	}
}
