package skills

import (
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// ScanBundle runs the same injection scanner internal/tools/admit.go uses
// for a tool descriptor (tools.Scan, reused rather than duplicated — it is
// already general over a Descriptor{Description} shape, and duplicating its
// regex list here would just be a second copy to drift) over every text
// field in the bundle: the manifest text (description/trigger hint) and
// every reference file's content. The worst verdict across all of them
// wins, matching tools.Registry's own "clean unless anything says
// otherwise" posture.
func ScanBundle(b SkillBundle) (tools.AdmissionStatus, []string) {
	worst := tools.AdmissionClean
	var findings []string

	scan := func(field, text string) {
		status, fs := tools.Scan(tools.Descriptor{Description: text})
		if worse(status, worst) {
			worst = status
		}
		for _, f := range fs {
			findings = append(findings, fmt.Sprintf("%s: %s", field, f.Pattern))
		}
	}

	scan("description", b.Description)
	scan("trigger_hint", b.TriggerHint)
	for _, f := range b.Files {
		scan("file:"+f.Path, string(f.Content))
	}

	return worst, findings
}

// worse reports whether candidate is a stricter verdict than current —
// pending < clean < flagged < rejected, though ScanBundle never produces
// pending (screening is always synchronous, exactly like memory.Screen).
func worse(candidate, current tools.AdmissionStatus) bool {
	rank := map[tools.AdmissionStatus]int{
		tools.AdmissionPending:  0,
		tools.AdmissionClean:    1,
		tools.AdmissionFlagged:  2,
		tools.AdmissionRejected: 3,
	}
	return rank[candidate] > rank[current]
}
