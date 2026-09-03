package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

type fakeSearcher struct {
	chunks []RetrievedChunk
	err    error

	gotTenantID, gotSessionID uuid.UUID
	gotQuery                  string
	gotTopK                   int
}

func (f *fakeSearcher) Search(_ context.Context, tenantID, sessionID uuid.UUID, query string, topK int) ([]RetrievedChunk, error) {
	f.gotTenantID, f.gotSessionID, f.gotQuery, f.gotTopK = tenantID, sessionID, query, topK
	return f.chunks, f.err
}

func TestRetrieve_Taint(t *testing.T) {
	taint := Retrieve{}.Taint()
	if !taint.ReadsPrivateData {
		t.Error("expected ReadsPrivateData=true (task 12.6)")
	}
	if !taint.ReturnsUntrusted {
		t.Error("expected ReturnsUntrusted=true (retrieved content is untrusted, Principle V)")
	}
	if taint.MutatesExternal {
		t.Error("expected MutatesExternal=false (a search has no external effect)")
	}
}

func TestRetrieve_ValidateInput(t *testing.T) {
	r := Retrieve{}
	if err := r.ValidateInput(context.Background(), json.RawMessage(`{"query":"revenue"}`), tools.RunContext{}); err != nil {
		t.Errorf("expected a valid query to pass, got %v", err)
	}
	if err := r.ValidateInput(context.Background(), json.RawMessage(`{}`), tools.RunContext{}); err == nil {
		t.Error("expected an empty query to be rejected")
	}
	if err := r.ValidateInput(context.Background(), json.RawMessage(`{"query":"x","top_k":-1}`), tools.RunContext{}); err == nil {
		t.Error("expected a negative top_k to be rejected")
	}
}

func TestRetrieve_Call_NotWired(t *testing.T) {
	r := Retrieve{}
	res, err := r.Call(context.Background(), json.RawMessage(`{"query":"x"}`), tools.RunContext{})
	if err != nil {
		t.Fatalf("Call returned an error rather than a tool-level failure: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError=true when no Searcher is wired")
	}
}

func TestRetrieve_Call_DelegatesToSearcher(t *testing.T) {
	tenantID, sessionID := uuid.New(), uuid.New()
	searcher := &fakeSearcher{chunks: []RetrievedChunk{{DocID: "d1", ChunkID: "c1", Content: "hello"}}}
	r := Retrieve{Searcher: searcher}

	res, err := r.Call(context.Background(), json.RawMessage(`{"query":"revenue","top_k":3}`), tools.RunContext{TenantID: tenantID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", res.Reason)
	}
	if searcher.gotTenantID != tenantID || searcher.gotSessionID != sessionID {
		t.Error("expected the tool to forward the caller's own tenant/session, never a value from input")
	}
	if searcher.gotQuery != "revenue" || searcher.gotTopK != 3 {
		t.Errorf("query/top_k not forwarded correctly: got %q/%d", searcher.gotQuery, searcher.gotTopK)
	}

	var out struct {
		Chunks []RetrievedChunk `json:"chunks"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(out.Chunks) != 1 || out.Chunks[0].Content != "hello" {
		t.Errorf("unexpected chunks in output: %+v", out.Chunks)
	}
}

func TestRetrieve_Call_SearcherErrorIsToolLevel(t *testing.T) {
	r := Retrieve{Searcher: &fakeSearcher{err: errors.New("index unavailable")}}
	res, err := r.Call(context.Background(), json.RawMessage(`{"query":"x"}`), tools.RunContext{})
	if err != nil {
		t.Fatalf("expected a tool-level failure, not a Go error: %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError=true when the searcher fails")
	}
}
