package runstate

import (
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// ConfigFor is the configuration a resumed run takes from its session: base (the
// system prompt, tool catalog and turn cap the resuming service holds for the
// whole process) with every field the session pinned at creation set from the
// session row.
//
//   - AutonomyLevel is the permission chain's layer 3 and only ever tightens.
//   - ModelID is the model the session was routed to, which the cost ledger prices
//     each turn under and the events record.
//   - Conversational decides whether a plain reply pauses the session for the
//     next message or ends it.
//
// The session's value always wins over whatever base holds, because a resume
// continues the run the session started rather than configuring a new one. A
// service that resumes a run takes those fields here instead of setting them in
// its own literal: each used to, and each took a different subset (oversight
// dropped the model and conversational, delegate and plan dropped conversational),
// so a conversational chat resumed after an approval ended instead of waiting for
// the next message. tests/contract/runconfig_test.go keeps a hand-set field from
// coming back, and this package's test fails when kernel.RunConfig gains a field
// nobody has decided how to resume.
//
// A child that delegate or teams spawns calls it too, with the session it has just
// written, so the row and the config of the run that executes it are one fact
// rather than two copies. The one run that does not is a fresh one started by a
// request (cmd/nexusd's kernelRunStarter), which takes the values from that
// request, the same ones the session was admitted with. Fields only a fresh run
// reads (Input, LoadedTools, MemorySources, MemoryText, ExtraCatalog) are never set
// here. The last two are what makes a session's prompt differ from the process-wide
// one; a resumed run gets them from the session's log through RunState.Pinned, which
// Load fills with kernel.RehydratePinned and the turn loop applies.
func ConfigFor(sess eventlog.Session, base kernel.RunConfig) kernel.RunConfig {
	base.AutonomyLevel = sess.AutonomyLevel
	base.ModelID = sess.RouteModelID
	base.Conversational = sess.Conversational
	return base
}
