package audit

import (
	"github.com/google/uuid"
)

// Break is one chain or anchor hash/signature mismatch — a scheduled
// verifier (task 5.3's "alerting on a break") treats any non-empty
// Report.Breaks as the alert condition.
type Break struct {
	SessionID uuid.UUID // zero for an anchor-level break
	Seq       int64     // meaningful only for a receipt break
	Kind      string    // "hash_mismatch" | "signature_invalid" | "unverifiable_signer_key"
	Detail    string
}

// Gap is one event with no matching receipt — "alerting on ... a sequence
// gap" (task 5.3): an event that reached the log without the chain being
// extended for it, which must never happen given kernel.Kernel.Receipts
// runs in the same transaction as the append, but is exactly the class of
// bug (or tamper) a verifier exists to catch even so.
type Gap struct {
	SessionID  uuid.UUID
	MissingSeq int64
}

// Report is Verify's answer for one tenant.
type Report struct {
	TenantID        uuid.UUID
	ReceiptsChecked int
	AnchorsChecked  int
	Breaks          []Break
	Gaps            []Gap
}

// OK reports whether the chain verified clean — no break, no gap.
func (r Report) OK() bool { return len(r.Breaks) == 0 && len(r.Gaps) == 0 }
