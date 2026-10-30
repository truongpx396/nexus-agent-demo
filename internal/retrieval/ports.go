package retrieval

import (
	"context"

	"github.com/google/uuid"
)

// Index is the tenant-scoped vector index a Retriever writes to and searches.
// Every method runs in tenantID's scope (README task 12.3's "no exception for
// embeddings"); the pgvector implementation is internal/retrieval/postgres.
type Index interface {
	// InsertDocument records one ingested source file, before any of its
	// chunks are indexed (so a chunk's doc_id always resolves).
	InsertDocument(ctx context.Context, tenantID uuid.UUID, d Document) (uuid.UUID, error)

	// IndexChunks stores chunks under docID. Callers have already filtered to
	// admission-clean chunks (internal/ingest.Admit); the index does not
	// re-check injection screening.
	IndexChunks(ctx context.Context, tenantID, docID uuid.UUID, chunks []ChunkRow) error

	// Search returns the topK chunks nearest queryEmbedding by cosine
	// distance, closest first.
	Search(ctx context.Context, tenantID uuid.UUID, queryEmbedding []float32, topK int) ([]ScoredChunk, error)
}
