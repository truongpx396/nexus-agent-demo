package store

import "github.com/google/uuid"

// Condensation is the third of the three state artifacts README §6 names —
// the structured-compaction output internal/promptctx/condense.go (Phase 7,
// task 7.11) will actually produce and seal into an EventCondensation
// payload. It is declared here, now, purely as a VALUE type (no table, no
// append helper — sealing needs internal/crypto, and this package cannot
// import that without a cycle; the real caller seals and appends it exactly
// the way kernel/loop.go's own contentPayload rides inside EventContent).
//
// Summary is model-facing prose: what condensation is FOR (task 6.5 — "model-
// facing only"). Deliberately absent: any field shaped like "did the
// external effect complete," "is_error," or a per-tool-call outcome ledger.
// That is by construction, not merely by convention — condensation_test.go
// proves it structurally (over this type's own JSON shape) rather than
// merely asserting it in prose, so a future field added here without also
// updating that test fails loudly.
type Condensation struct {
	CondensationID    uuid.UUID
	CoveredThroughSeq int64
	Summary           string
}

// Condense builds a Condensation over everything up to and including
// throughSeq. summary is the caller's already-produced model-facing prose
// (Phase 7's condenser call produces it via the Provider port); this
// function's whole job is to be the ONE place a Condensation value gets
// constructed, so "what fields does a condensation carry" has one answer.
func Condense(throughSeq int64, summary string) Condensation {
	return Condensation{CondensationID: uuid.New(), CoveredThroughSeq: throughSeq, Summary: summary}
}
