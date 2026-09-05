package oversight

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// TestErrApprovalNotPending_Error is README task 13.8 (closing
// production-readiness finding F9): internal/oversight shipped with zero
// _test.go files despite owning the approval transaction (README pattern
// #23). Names the approval id and its actual (non-pending) status in the
// error text — what a human/log line sees when a second decision is
// attempted on an already-decided approval.
func TestErrApprovalNotPending_Error(t *testing.T) {
	id := uuid.New()
	err := ErrApprovalNotPending{ApprovalID: id, Status: ApprovalGranted}
	msg := err.Error()
	for _, want := range []string{id.String(), string(ApprovalGranted), "not pending"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}
}

func TestNullIfEmpty(t *testing.T) {
	if got := nullIfEmpty(""); got != nil {
		t.Errorf("nullIfEmpty(\"\") = %v, want nil", got)
	}
	got := nullIfEmpty("operator@example.com")
	if got == nil || *got != "operator@example.com" {
		t.Errorf("nullIfEmpty(...) = %v, want a pointer to the input", got)
	}
}

// TestCurrentActiveKeyID is README task 13.8: the pure scan
// Resumer.loadRunState uses to find the most recent non-erasure key id in a
// session's history — walks backward so a key rotated mid-session resolves
// to the CURRENT key, not the first one ever used.
func TestCurrentActiveKeyID(t *testing.T) {
	t.Run("finds the most recent non-erasure key", func(t *testing.T) {
		history := []store.Event{
			{KeyID: "key-1"},
			{KeyID: "key-2"},
			{KeyID: "key-3"},
		}
		got, err := currentActiveKeyID(history)
		if err != nil {
			t.Fatalf("currentActiveKeyID: %v", err)
		}
		if got != "key-3" {
			t.Errorf("got %q, want %q (the most recent)", got, "key-3")
		}
	})

	t.Run("skips a trailing erasure key", func(t *testing.T) {
		history := []store.Event{
			{KeyID: "key-1"},
			{KeyID: "key-2"},
			{KeyID: crypto.ErasureKeyID},
		}
		got, err := currentActiveKeyID(history)
		if err != nil {
			t.Fatalf("currentActiveKeyID: %v", err)
		}
		if got != "key-2" {
			t.Errorf("got %q, want %q (the last non-erasure key)", got, "key-2")
		}
	})

	t.Run("errors when every key is the erasure sentinel", func(t *testing.T) {
		history := []store.Event{{KeyID: crypto.ErasureKeyID}}
		if _, err := currentActiveKeyID(history); err == nil {
			t.Error("expected an error when no active key exists in history")
		}
	})

	t.Run("errors on empty history", func(t *testing.T) {
		if _, err := currentActiveKeyID(nil); err == nil {
			t.Error("expected an error for empty history")
		}
	})
}

// TestErrSeq proves errSeq's one-shot iterator yields exactly the given
// error once and then stops — the shape Grant/GrantModified/Deny return
// on an early failure, before any real kernel.Resume ever runs.
func TestErrSeq(t *testing.T) {
	sentinel := ErrNotFound
	seq := errSeq(sentinel)
	var count int
	for _, err := range seq {
		count++
		if err != sentinel {
			t.Errorf("yielded error = %v, want %v", err, sentinel)
		}
	}
	if count != 1 {
		t.Errorf("errSeq yielded %d times, want exactly 1", count)
	}
}
