package runctl

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
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

// TestCurrentActiveKeyID mirrors internal/oversight's own test for the
// identically-shaped helper in this package (runctl/rehydrate.go) — each
// out-of-band rehydrator duplicates this scan rather than sharing it (that
// file's own doc comment explains why), so each copy gets its own test.
func TestCurrentActiveKeyID(t *testing.T) {
	t.Run("finds the most recent non-erasure key", func(t *testing.T) {
		history := []store.Event{{KeyID: "key-1"}, {KeyID: "key-2"}}
		got, err := currentActiveKeyID(history)
		if err != nil {
			t.Fatalf("currentActiveKeyID: %v", err)
		}
		if got != "key-2" {
			t.Errorf("got %q, want %q", got, "key-2")
		}
	})

	t.Run("skips a trailing erasure key", func(t *testing.T) {
		history := []store.Event{{KeyID: "key-1"}, {KeyID: crypto.ErasureKeyID}}
		got, err := currentActiveKeyID(history)
		if err != nil {
			t.Fatalf("currentActiveKeyID: %v", err)
		}
		if got != "key-1" {
			t.Errorf("got %q, want %q", got, "key-1")
		}
	})

	t.Run("errors when no active key exists", func(t *testing.T) {
		if _, err := currentActiveKeyID([]store.Event{{KeyID: crypto.ErasureKeyID}}); err == nil {
			t.Error("expected an error when every key is the erasure sentinel")
		}
	})
}
