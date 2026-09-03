// Package ingest is document conversion (README Phase 12, task 12.1):
// PDF/DOCX/HTML/plaintext normalized to plain text, chunked with
// declared, stable boundaries, and scanned before internal/retrieval ever
// indexes a byte of it (task 12.2). Nothing here talks to Postgres,
// pgvector, or an embedding model — this package produces plain Go values;
// internal/retrieval is what turns them into an index.
package ingest

import "github.com/truongpx396/nexus-agent-demo/internal/tools"

// Document is one converted source file: its normalized text plus the
// identity a caller needs to trace a chunk back to where it came from.
type Document struct {
	SourceName   string
	MimeType     string
	Text         string
	SourceDigest []byte // sha256 over the RAW input bytes (internal/crypto.Digest) — task 12.1's "deterministic per-document digest", taken before conversion so it identifies the source, not this package's own lossy extraction of it
}

// Chunk is one bounded slice of a Document's text, in reading order.
type Chunk struct {
	Index   int
	Text    string
	Status  tools.AdmissionStatus // Admit's verdict for this chunk specifically
	Finding []string
}
