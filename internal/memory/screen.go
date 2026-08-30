package memory

import (
	"regexp"
	"strings"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// Status is memory's own three-valued screening verdict — no "pending" state
// exists here (unlike tools.AdmissionStatus) because screening a memory file
// is always synchronous, done at Load time, never deferred.
type Status string

const (
	StatusClean    Status = "clean"
	StatusFlagged  Status = "flagged"
	StatusRejected Status = "rejected"
)

// exfiltrationPatterns catch secrets that have no business living in
// durable, cross-session memory — the half of FR-019's "injection/
// exfiltration screening" tools.Scan doesn't cover (tools.Scan is about a
// descriptor talking the model into something; this is about memory content
// smuggling a credential into every future session's prompt). Demo-scoped
// illustrative policy, same caveat as tools.Scan's own patterns.
var exfiltrationPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`),            // OpenAI/Anthropic-shaped API key
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),               // AWS access key id
	regexp.MustCompile(`(?i)\bbearer [A-Za-z0-9._-]{20,}\b`), // bearer token
}

// Screen scans one memory file's text before it is ever injected into a
// prompt: tools.Scan's injection-phrase patterns (reused, not duplicated —
// tools.Scan is already general over a Descriptor{Description} shape) worst-
// cased against this package's own exfiltration patterns. Anything but
// StatusClean is fail-closed: never injected (Load's own doc comment).
func Screen(text string) (Status, []string) {
	if strings.TrimSpace(text) == "" {
		return StatusClean, nil // an empty memory file is boring, not suspicious — unlike an empty tool descriptor
	}

	var findings []string

	scanStatus, scanFindings := tools.Scan(tools.Descriptor{Description: text})
	for _, f := range scanFindings {
		findings = append(findings, "injection: "+f.Pattern)
	}

	for _, p := range exfiltrationPatterns {
		if p.MatchString(text) {
			findings = append(findings, "exfiltration: "+p.String())
		}
	}

	switch {
	case scanStatus == tools.AdmissionRejected || containsExfiltration(findings):
		return StatusRejected, findings
	case scanStatus == tools.AdmissionFlagged || len(findings) > 0:
		return StatusFlagged, findings
	default:
		return StatusClean, findings
	}
}

func containsExfiltration(findings []string) bool {
	for _, f := range findings {
		if len(f) >= len("exfiltration:") && f[:len("exfiltration:")] == "exfiltration:" {
			return true
		}
	}
	return false
}
