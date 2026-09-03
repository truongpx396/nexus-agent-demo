package ingest

import (
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// Admit runs the same injection scanner internal/tools/admit.go uses for a
// tool descriptor (tools.Scan, reused rather than duplicated — the same
// choice internal/skills.ScanBundle and internal/memory.Screen already
// made) over every chunk of a converted Document, before
// internal/retrieval ever indexes one (task 12.2: "the same admission-scan
// discipline as a skill bundle or board card ... before a chunk is ever
// indexed"). It returns the document's own worst-case verdict across all
// its chunks plus each chunk's individual verdict — a caller indexes only
// the chunks that came back AdmissionClean, mirroring a flagged board
// card's "never surfaced to another peer's context" (internal/tools/
// builtin/board.go).
func Admit(doc Document) (tools.AdmissionStatus, []Chunk) {
	texts := SplitText(doc.Text)
	chunks := make([]Chunk, len(texts))
	worst := tools.AdmissionClean

	for i, text := range texts {
		status, findings := tools.Scan(tools.Descriptor{Description: text})
		var reasons []string
		for _, f := range findings {
			reasons = append(reasons, fmt.Sprintf("%s: %s", f.Field, f.Pattern))
		}
		chunks[i] = Chunk{Index: i, Text: text, Status: status, Finding: reasons}
		if worse(status, worst) {
			worst = status
		}
	}
	if len(texts) == 0 {
		// An empty document scans clean the same way tools.Scan treats an
		// empty descriptor field elsewhere — "boring, not suspicious"
		// (internal/memory/screen.go's own phrase for the same case).
		return tools.AdmissionClean, nil
	}
	return worst, chunks
}

// worse mirrors internal/skills/admit.go's own ranking — duplicated rather
// than exported from there, since neither package should depend on the
// other for one small ordering helper.
func worse(candidate, current tools.AdmissionStatus) bool {
	rank := map[tools.AdmissionStatus]int{
		tools.AdmissionPending:  0,
		tools.AdmissionClean:    1,
		tools.AdmissionFlagged:  2,
		tools.AdmissionRejected: 3,
	}
	return rank[candidate] > rank[current]
}
