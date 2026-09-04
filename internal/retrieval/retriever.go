package retrieval

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/ingest"
	"github.com/truongpx396/nexus-agent-demo/internal/memory"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// TenantTxRunner is the one method this package needs from *store.Store —
// declared locally, structurally satisfied, the same decoupling idiom
// internal/tools.SandboxExec and internal/tools/builtin's own resolver
// interfaces already use, so this package's tests can fake it without a
// live Postgres.
type TenantTxRunner interface {
	InTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error
}

// EmbeddingBudget is the narrow slice of *internal/cost.Gate this package
// needs — the same structural-interface idiom kernel.BudgetGate already
// applies to the SAME concrete type (kernel/types.go), so Retriever depends
// on two methods instead of the whole Gate and a test can fake this without
// a live Postgres/Redis pair.
type EmbeddingBudget interface {
	Reserve(ctx context.Context, req cost.ReserveRequest) (cost.Reservation, error)
	ReconcileEmbedding(ctx context.Context, res cost.Reservation, tokensUsed int, reported bool) error
}

// Retriever is task 12.4's own orchestration: every embedding call goes
// through Gate.Reserve BEFORE Embedder.Embed and Gate.ReconcileEmbedding
// AFTER it, exactly the reserve-then-reconcile shape kernel/loop.go already
// runs for a chat call — "indexing is metered, never off the paying loop."
// It also owns document ingestion (IndexDocument): converting, chunking,
// and admission-scanning a document (internal/ingest) live in a separate
// package because THAT part talks to no Postgres or embedding model at
// all, but the two are stitched together here rather than at the wiring
// layer (cmd/nexusd) for the same reason internal/teams.Service — not
// cmd/nexusd — is where board-card scanning happens: an operation this
// stateful belongs in a service, not scattered across main.go.
type Retriever struct {
	Store    TenantTxRunner
	Gate     EmbeddingBudget
	Embedder provider.Embedder
	// ModelID attributes every reservation/cost record this Retriever makes
	// (cost.ReserveRequest.ModelID) — a label for the price book to key off
	// (internal/cost/pricebook.go's own per-subject override feature), not
	// a live credential or endpoint selector; internal/provider/fake is the
	// only Embedder this demo ships, so in practice this is just a display
	// name like "fake-embedder-v1".
	ModelID string
	TopK    int // default result count for Search; <=0 falls back to Search's own default of 5
}

// IndexDocument runs task 12.1's conversion, task 12.2's admission scan,
// and task 12.3's indexing as one pipeline: a document whose OVERALL scan
// verdict is anything but clean gets a documents row recording that (so an
// operator can see WHY nothing was indexed) but contributes zero chunks —
// the same fail-closed "never surfaced" posture a flagged board card gets
// (internal/tools/builtin/board.go).
func (r *Retriever) IndexDocument(ctx context.Context, tenantID, sessionID uuid.UUID, sourceName, mimeType string, data []byte) (Document, error) {
	doc, err := ingest.Convert(sourceName, mimeType, data)
	if err != nil {
		return Document{}, err
	}
	status, chunks := ingest.Admit(doc)

	docRow := Document{
		TenantID: tenantID, SourceName: doc.SourceName, MimeType: doc.MimeType,
		SourceDigest: doc.SourceDigest, ChunkCount: len(chunks), AdmissionStatus: string(status),
	}
	for _, c := range chunks {
		docRow.AdmissionFindings = append(docRow.AdmissionFindings, c.Finding...)
	}

	var docID uuid.UUID
	err = r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var ierr error
		docID, ierr = InsertDocument(ctx, tx, tenantID, docRow)
		return ierr
	})
	if err != nil {
		return Document{}, err
	}
	docRow.DocID = docID

	if status != tools.AdmissionClean {
		return docRow, nil // fail closed: recorded, but nothing gets indexed
	}

	var texts []string
	for _, c := range chunks {
		if c.Status != tools.AdmissionClean {
			continue // a document can be overall-clean with zero rejected chunks by construction (Admit's own worst-of-all rule) — this branch only ever matters if that rule changes
		}
		texts = append(texts, c.Text)
	}
	if len(texts) == 0 {
		return docRow, nil
	}

	embeddings, err := r.embed(ctx, tenantID, sessionID, texts)
	if err != nil {
		return Document{}, fmt.Errorf("retrieval: embed document %s: %w", sourceName, err)
	}

	rows := make([]ChunkRow, len(texts))
	for i, text := range texts {
		rows[i] = ChunkRow{
			DocID: docID, ChunkIndex: i, Content: text,
			Embedding: embeddings[i], SourceDigest: doc.SourceDigest, InjectionScanStatus: "clean",
		}
	}
	err = r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return IndexChunks(ctx, tx, tenantID, docID, rows)
	})
	if err != nil {
		return Document{}, err
	}
	return docRow, nil
}

// Search embeds query (metered exactly like IndexDocument's own embedding
// calls) and returns the nearest chunks, re-screened through
// internal/memory.Screen — task 12.7's "retrieved chunks pass through the
// same injection-screening as memory before entering the prompt," a second,
// independent gate on top of ingest-time admission (defense in depth, the
// same layering discipline the permission chain's own "layers 6 and 7 are
// unconditional" rule applies to a completely different mechanism). A
// chunk that fails this re-screen is dropped, never returned redacted —
// there is no legitimate reason for the model to see even a placeholder
// for content the store itself no longer trusts.
func (r *Retriever) Search(ctx context.Context, tenantID, sessionID uuid.UUID, query string, topK int) ([]ScoredChunk, error) {
	embeddings, err := r.embed(ctx, tenantID, sessionID, []string{query})
	if err != nil {
		return nil, fmt.Errorf("retrieval: embed query: %w", err)
	}
	if topK <= 0 {
		topK = r.TopK
	}

	var results []ScoredChunk
	err = r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var serr error
		results, serr = Search(ctx, tx, tenantID, embeddings[0], topK)
		return serr
	})
	if err != nil {
		return nil, err
	}

	return screenClean(results), nil
}

// screenClean is Search's re-screening filter (task 12.7), pulled out as
// its own pure function so it is unit-testable without a live Postgres —
// the rest of Search is a thin embed-then-query wrapper around this and
// internal/retrieval's own SQL, exercised together in
// tests/integration/phase12_retrieval_test.go.
func screenClean(results []ScoredChunk) []ScoredChunk {
	clean := make([]ScoredChunk, 0, len(results))
	for _, c := range results {
		if status, _ := memory.Screen(c.Content); status == memory.StatusClean {
			clean = append(clean, c)
		}
	}
	return clean
}

// embed is IndexDocument and Search's shared reserve-then-reconcile call:
// one Gate.Reserve(PurposeEmbedding) before Embedder.Embed, one
// Gate.ReconcileEmbedding after — the call site
// tests/contract/embedding_metering_test.go's AST check verifies is never
// bypassed. A refused reservation (cost_exhausted-shaped) fails the whole
// call before Embed is ever reached; an Embed that itself errors still
// reconciles as UNREPORTED (reported=false) rather than leaving the
// reservation's worst case sitting uncharged forever.
func (r *Retriever) embed(ctx context.Context, tenantID, sessionID uuid.UUID, texts []string) ([]provider.Embedding, error) {
	res, err := r.Gate.Reserve(ctx, cost.ReserveRequest{
		TenantID: tenantID, SessionID: sessionID, ModelID: r.ModelID, Purpose: cost.PurposeEmbedding,
	})
	if err != nil {
		return nil, fmt.Errorf("embedding reservation refused: %w", err)
	}

	embeddings, usage, embedErr := r.Embedder.Embed(ctx, texts, provider.RunContext{TenantID: tenantID, SessionID: sessionID})
	reported := embedErr == nil
	if rerr := r.Gate.ReconcileEmbedding(ctx, res, usage.Tokens, reported); rerr != nil {
		slog.Error("retrieval: failed to reconcile embedding reservation", "error", rerr, "tenant_id", tenantID, "session_id", sessionID)
	}
	if embedErr != nil {
		return nil, embedErr
	}
	if len(embeddings) != len(texts) {
		return nil, fmt.Errorf("embedder returned %d vectors for %d texts", len(embeddings), len(texts))
	}
	return embeddings, nil
}

// Erase is task 12.8's own gate, called from the SAME transaction as
// internal/crypto.EraseTenant (cmd/nexusd's `erase --tenant` path) — see
// DeleteTenant's own doc comment for why this package never imports
// internal/crypto to do that itself.
func Erase(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (deletedChunks, deletedDocs int, err error) {
	return DeleteTenant(ctx, tx, tenantID)
}

var (
	_ TenantTxRunner  = (*store.Store)(nil)
	_ EmbeddingBudget = (*cost.Gate)(nil)
)
