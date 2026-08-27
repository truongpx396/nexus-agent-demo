// Command signerd holds the sign-only audit-chain signing key over a unix
// socket: nexusd may ask it to sign a digest, but can never read the key
// itself (README.md §5, Phase 5). It is a scaffold today.
package main

import (
	"fmt"
	"os"

	"github.com/truongpx396/nexus-agent-demo/internal/version"
)

func main() {
	if _, err := fmt.Fprintf(os.Stdout, "signerd %s (%s)\n", version.Version, version.GitCommit); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprintln(os.Stdout, "scaffold only — sign-only key custody lands in Phase 5 (see README.md)"); err != nil {
		os.Exit(1)
	}
}
