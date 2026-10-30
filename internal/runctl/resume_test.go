package runctl

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestErrUnresolvedClaims_Error is README task 13.8 (closing
// production-readiness finding F9): internal/runctl shipped with zero
// _test.go files despite owning replay/resume/fork (README pattern #27).
// Names the session and how many claims block the resume — what a caller
// sees when Resume refuses to proceed past an in-flight claim.
func TestErrUnresolvedClaims_Error(t *testing.T) {
	sessionID := uuid.New()
	err := ErrUnresolvedClaims{SessionID: sessionID, ClaimIDs: []uuid.UUID{uuid.New(), uuid.New()}}
	msg := err.Error()
	for _, want := range []string{sessionID.String(), "2 unresolved claim"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}
}
