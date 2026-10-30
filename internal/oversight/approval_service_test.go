package oversight_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

var (
	tenant = uuid.New()
	future = time.Now().Add(time.Hour)
	past   = time.Now().Add(-time.Hour)
)

func newApprovals() (*oversight.Approvals, *fakeStore) {
	f := newFakeStore()
	return oversight.NewApprovals(f), f
}

func TestCreate_DefaultsTheAssigneeToTheSessionsOwner(t *testing.T) {
	a, f := newApprovals()
	session, owner := uuid.New(), uuid.New()
	f.owners[session] = owner

	ap, err := a.Create(context.Background(), oversight.CreateApprovalRequest{TenantID: tenant, SessionID: session, ToolUseEventID: uuid.New(), ToolID: "platform/shell@v1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ap.Assignee != owner.String() {
		t.Errorf("assignee = %q, want the session's owner %s: with no approver directory, whoever owns the run is the only honest default", ap.Assignee, owner)
	}
	if ap.Status != oversight.ApprovalPending || !ap.ExpiresAt.After(time.Now()) {
		t.Errorf("approval = status %s, expires %v, want pending with a TTL in the future", ap.Status, ap.ExpiresAt)
	}

	named, err := a.Create(context.Background(), oversight.CreateApprovalRequest{TenantID: tenant, SessionID: session, ToolUseEventID: uuid.New(), ToolID: "t", Assignee: "ops@example.com"})
	if err != nil || named.Assignee != "ops@example.com" {
		t.Errorf("explicit assignee = %q, %v, want it kept", named.Assignee, err)
	}
}

func TestGrant_ChangesTheStatusAndRecordsItsEventTogether(t *testing.T) {
	a, f := newApprovals()
	session := uuid.New()
	pending := f.seedApproval(session, future)

	got, err := a.Grant(context.Background(), tenant, pending.ApprovalID, "alice")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if got.Status != oversight.ApprovalGranted || f.approvals[pending.ApprovalID].DecidedBy != "alice" {
		t.Errorf("approval = %s decided by %q, want granted by alice", got.Status, f.approvals[pending.ApprovalID].DecidedBy)
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventApprovalGranted {
		t.Fatalf("events = %v, want exactly approval_granted", types)
	}
	if ev := f.events[0]; ev.SessionID != session || ev.ToolID == nil || *ev.ToolID != "platform/shell@v1" {
		t.Errorf("event = %+v, want it on the approval's session carrying its tool id", ev)
	}
}

func TestADecisionOnAnApprovalThatIsAlreadyDecidedIsRefused(t *testing.T) {
	a, f := newApprovals()
	pending := f.seedApproval(uuid.New(), future)
	ctx := context.Background()
	if _, err := a.Grant(ctx, tenant, pending.ApprovalID, "alice"); err != nil {
		t.Fatalf("first Grant: %v", err)
	}

	_, err := a.Deny(ctx, tenant, pending.ApprovalID, "mallory", "changed my mind")
	var notPending oversight.ErrApprovalNotPending
	if !errors.As(err, &notPending) || notPending.Status != oversight.ApprovalGranted {
		t.Fatalf("second decision = %v, want ErrApprovalNotPending with status granted", err)
	}
	if f.approvals[pending.ApprovalID].Status != oversight.ApprovalGranted {
		t.Error("the second decision overwrote the first")
	}
	if len(f.events) != 1 {
		t.Errorf("%d events after a refused second decision, want only the first's", len(f.events))
	}
}

func TestGrantModified_ReBindsTheDigestToTheSubstitutedInput(t *testing.T) {
	a, f := newApprovals()
	pending := f.seedApproval(uuid.New(), future)
	modified := json.RawMessage(`{"to":"someone-else@example.com"}`)
	want, err := tools.CanonicalDigest(pending.ToolID, modified)
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}

	got, err := a.GrantModified(context.Background(), tenant, pending.ApprovalID, "alice", modified)
	if err != nil {
		t.Fatalf("GrantModified: %v", err)
	}
	if got.Status != oversight.ApprovalGrantedModified || !bytes.Equal(got.CanonicalDigest, want) || bytes.Equal(got.CanonicalDigest, pending.CanonicalDigest) {
		t.Errorf("approval = %s, digest %x; want granted_modified with the digest of the NEW input, not the original", got.Status, got.CanonicalDigest)
	}
	stored := f.approvals[pending.ApprovalID]
	if !bytes.Equal(stored.CanonicalDigest, want) || !bytes.Equal(stored.GrantedInput, modified) {
		t.Errorf("stored digest %x / input %s, want the rebound digest and the substituted input persisted", stored.CanonicalDigest, stored.GrantedInput)
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventApprovalGrantedModified {
		t.Errorf("events = %v, want approval_granted_modified", types)
	}
}

func TestDeny_RecordsTheReasonOnTheApprovalAndInItsEvent(t *testing.T) {
	a, f := newApprovals()
	pending := f.seedApproval(uuid.New(), future)

	got, err := a.Deny(context.Background(), tenant, pending.ApprovalID, "alice", "wrong recipient")
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if got.Status != oversight.ApprovalDenied || got.Reason != "wrong recipient" || f.approvals[pending.ApprovalID].Reason != "wrong recipient" {
		t.Errorf("approval = %s reason %q, want denied with the reason persisted", got.Status, got.Reason)
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventApprovalDenied {
		t.Errorf("events = %v, want approval_denied", types)
	}
}

func TestAnApprovalPastItsTTLIsRefusedEvenIfNothingSweptItYet(t *testing.T) {
	a, f := newApprovals()
	pending := f.seedApproval(uuid.New(), past)

	_, err := a.Grant(context.Background(), tenant, pending.ApprovalID, "alice")
	var notPending oversight.ErrApprovalNotPending
	if !errors.As(err, &notPending) || notPending.Status != oversight.ApprovalExpired {
		t.Fatalf("Grant after expiry = %v, want ErrApprovalNotPending with status expired", err)
	}
	if len(f.events) != 0 {
		t.Errorf("%d events for a refused grant, want none", len(f.events))
	}
}

func TestADecisionIsAllOrNothing(t *testing.T) {
	a, f := newApprovals()
	pending := f.seedApproval(uuid.New(), future)
	f.failAppend = errors.New("audit chain unavailable")

	if _, err := a.Grant(context.Background(), tenant, pending.ApprovalID, "alice"); err == nil {
		t.Fatal("Grant succeeded though its event could not be recorded")
	}
	if f.approvals[pending.ApprovalID].Status != oversight.ApprovalPending {
		t.Errorf("status = %s after a failed grant, want it still pending: a status must never change without its event", f.approvals[pending.ApprovalID].Status)
	}
}

func TestAnUnknownApprovalIsNotFound(t *testing.T) {
	a, _ := newApprovals()
	if _, err := a.Grant(context.Background(), tenant, uuid.New(), "alice"); !errors.Is(err, oversight.ErrNotFound) {
		t.Errorf("Grant of an unknown approval = %v, want ErrNotFound", err)
	}
	if _, err := a.Get(context.Background(), tenant, uuid.New()); !errors.Is(err, oversight.ErrNotFound) {
		t.Errorf("Get of an unknown approval = %v, want ErrNotFound", err)
	}
}

func TestListPending_ReturnsOnlyWhatIsStillPending(t *testing.T) {
	a, f := newApprovals()
	first, second := f.seedApproval(uuid.New(), future), f.seedApproval(uuid.New(), future)
	if _, err := a.Deny(context.Background(), tenant, first.ApprovalID, "alice", ""); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	got, err := a.ListPending(context.Background(), tenant)
	if err != nil || len(got) != 1 || got[0].ApprovalID != second.ApprovalID {
		t.Errorf("ListPending = %+v, %v, want only the undecided approval", got, err)
	}
}

func TestInvalidate_ReleasesThePendingApprovalWithAPairedSyntheticResult(t *testing.T) {
	a, f := newApprovals()
	session := uuid.New()
	pending := f.seedApproval(session, future)

	got, err := a.Invalidate(context.Background(), tenant, session, oversight.InvalidationCancel)
	if err != nil || got == nil {
		t.Fatalf("Invalidate = %v, %v, want the invalidated approval", got, err)
	}
	if got.Status != oversight.ApprovalInvalidated || f.approvals[pending.ApprovalID].Reason != string(oversight.InvalidationCancel) {
		t.Errorf("approval = %s reason %q, want invalidated with the trigger recorded", got.Status, f.approvals[pending.ApprovalID].Reason)
	}
	types := f.eventTypes()
	if len(types) != 2 || types[0] != eventlog.EventApprovalInvalidated || types[1] != eventlog.EventToolResult {
		t.Fatalf("events = %v, want approval_invalidated then a tool_result", types)
	}
	if result := f.events[1]; result.PairRef == nil || *result.PairRef != pending.ToolUseEventID {
		t.Errorf("tool_result pair ref = %v, want the gated tool_use %s: every tool_use must be paired", result.PairRef, pending.ToolUseEventID)
	}
}

func TestInvalidate_WithNothingPendingIsANoOp(t *testing.T) {
	a, f := newApprovals()
	got, err := a.Invalidate(context.Background(), tenant, uuid.New(), oversight.InvalidationReap)
	if err != nil || got != nil || len(f.events) != 0 {
		t.Errorf("Invalidate = %v, %v with %d events, want (nil, nil) and no events: a trigger may fire against a session that never suspended", got, err, len(f.events))
	}
}
