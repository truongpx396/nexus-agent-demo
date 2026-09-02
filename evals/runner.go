package evals

import (
	"context"
	"fmt"
	"strings"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
)

// RunProviderScriptCases grades every case as a deterministic code check —
// no judge, no live model, because none of this is subjective (Phase 10's
// "a judge is the last resort, not the default" rule, satisfied trivially
// here since every case in this suite IS objectively checkable).
func RunProviderScriptCases(cases []ProviderScriptCase) Report {
	var report Report
	for _, c := range cases {
		report.Trials = append(report.Trials, runOne(c))
	}
	return report
}

func runOne(c ProviderScriptCase) Trial {
	p := fake.New(c.Script)
	stream, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		return gradeAgainstError(c, err)
	}

	var text strings.Builder
	var done provider.DoneReason
	for {
		chunk, ok, err := stream.Next(context.Background())
		if err != nil {
			return gradeAgainstError(c, err)
		}
		if !ok {
			break
		}
		switch chunk.Kind {
		case provider.ChunkContent:
			text.WriteString(chunk.Text)
		case provider.ChunkReasoning, provider.ChunkToolUse, provider.ChunkUsage:
			// not graded by this suite — final_text/done are all it checks
		case provider.ChunkDone:
			done = chunk.Done
		}
	}

	if c.Want.ExpectError {
		return Trial{
			CaseID:  c.ID,
			Verdict: VerdictFail,
			Detail:  "expected an error draining the stream, got none",
		}
	}
	if text.String() != c.Want.FinalText || string(done) != c.Want.Done {
		return Trial{
			CaseID:  c.ID,
			Verdict: VerdictFail,
			Detail:  fmt.Sprintf("got text=%q done=%q, want text=%q done=%q", text.String(), done, c.Want.FinalText, c.Want.Done),
		}
	}
	return Trial{CaseID: c.ID, Verdict: VerdictPass}
}

func gradeAgainstError(c ProviderScriptCase, err error) Trial {
	if c.Want.ExpectError {
		return Trial{CaseID: c.ID, Verdict: VerdictPass, Detail: fmt.Sprintf("expected error: %v", err)}
	}
	return Trial{CaseID: c.ID, Verdict: VerdictFail, Detail: fmt.Sprintf("unexpected error: %v", err)}
}
