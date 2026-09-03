// Package retrieval is the pgvector index (README Phase 12, task 12.3): a
// second, durable knowledge store sitting BESIDE file-first memory
// (internal/memory), not instead of it — same tenant-scoping (task 12.3's
// "no exception for embeddings"), same fail-closed injection screening
// (task 12.7 reuses internal/memory's own Screen), same crypto-shredding
// obligation as any other derived, plaintext-adjacent artifact (task 12.8).
//
// This package owns three things: the pgvector-backed store
// (index/search/delete, store.go), the embedding+reservation orchestration
// a search query needs (retriever.go), and the vector encoding pgx has no
// native type for (vector.go). internal/ingest hands it Documents/chunks
// already converted, chunked, and admission-scanned — this package never
// converts or scans anything itself.
package retrieval

import (
	"time"

	"github.com/google/uuid"
)

// EmbeddingDimensions is the fixed vector width this demo's schema and
// embedder agree on — see internal/provider/fake.EmbeddingDimensions's own
// doc comment for why the two constants are declared independently rather
// than one importing the other, and migrations/0022_retrieval.sql's
// `vector(32)` column for the schema half of that agreement.
const EmbeddingDimensions = 32

// ChunkRow is one retrieval_chunks row.
type ChunkRow struct {
	ChunkID             uuid.UUID
	TenantID            uuid.UUID
	DocID               uuid.UUID
	ChunkIndex          int
	Content             string
	Embedding           []float32
	SourceDigest        []byte
	InjectionScanStatus string
	CreatedAt           time.Time
}

// ScoredChunk is one Search result: a ChunkRow plus its cosine distance
// from the query embedding (pgvector's `<=>` operator — smaller is more
// similar; 0 is an exact match, 2 is diametrically opposite for
// unit-normalized vectors).
type ScoredChunk struct {
	ChunkRow
	Distance float64
}

// Document mirrors one documents row.
type Document struct {
	DocID             uuid.UUID
	TenantID          uuid.UUID
	SourceName        string
	MimeType          string
	SourceDigest      []byte
	ChunkCount        int
	AdmissionStatus   string
	AdmissionFindings []string
	CreatedAt         time.Time
}
