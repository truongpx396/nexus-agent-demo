package retrieval

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// These take a caller-supplied tx, the same convention internal/cost's
// price-book access and internal/crypto.KeyStore's methods already follow:
// every tenant-scoped read/write in this system goes through
// store.Store.InTenantTx at the call site, never opens its own transaction
// (README §4's "InTenantTx ... the only scoping call in the codebase").

// InsertDocument writes one documents row — called once per ingested
// source file, before any of its chunks are indexed (so a chunk's
// doc_id foreign key always resolves).
func InsertDocument(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, d Document) (uuid.UUID, error) {
	if d.DocID == uuid.Nil {
		d.DocID = uuid.New()
	}
	findings := d.AdmissionFindings
	if findings == nil {
		findings = []string{}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO documents (
			doc_id, tenant_id, source_name, mime_type, source_digest,
			chunk_count, admission_status, admission_findings
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		d.DocID, tenantID, d.SourceName, d.MimeType, d.SourceDigest,
		d.ChunkCount, d.AdmissionStatus, findings,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("retrieval: insert document: %w", err)
	}
	return d.DocID, nil
}

// IndexChunks inserts every given ChunkRow. Callers are expected to have
// already filtered to admission-clean chunks (internal/ingest.Admit) —
// this function does not re-check InjectionScanStatus; it trusts its
// caller the same way internal/teams.Service.WriteCard's own DB layer
// trusts a status its own caller already computed.
func IndexChunks(ctx context.Context, tx pgx.Tx, tenantID, docID uuid.UUID, chunks []ChunkRow) error {
	for _, c := range chunks {
		if c.ChunkID == uuid.Nil {
			c.ChunkID = uuid.New()
		}
		if len(c.Embedding) != EmbeddingDimensions {
			return fmt.Errorf("retrieval: chunk %d has %d embedding dimensions, want %d", c.ChunkIndex, len(c.Embedding), EmbeddingDimensions)
		}
		status := c.InjectionScanStatus
		if status == "" {
			status = "clean"
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO retrieval_chunks (
				chunk_id, tenant_id, doc_id, chunk_index, content,
				embedding, source_digest, injection_scan_status
			) VALUES ($1,$2,$3,$4,$5,$6::vector,$7,$8)`,
			c.ChunkID, tenantID, docID, c.ChunkIndex, c.Content,
			formatVector(c.Embedding), c.SourceDigest, status,
		)
		if err != nil {
			return fmt.Errorf("retrieval: index chunk %d: %w", c.ChunkIndex, err)
		}
	}
	return nil
}

// Search returns the topK chunks nearest queryEmbedding by cosine distance
// (pgvector's `<=>` operator — task 12.3's own index), ordered closest
// first. It selects `embedding::text` explicitly (vector.go's own doc
// comment on why) rather than the raw vector column.
func Search(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, queryEmbedding []float32, topK int) ([]ScoredChunk, error) {
	if topK <= 0 {
		topK = 5
	}
	rows, err := tx.Query(ctx, `
		SELECT chunk_id, doc_id, chunk_index, content, embedding::text,
		       source_digest, injection_scan_status, created_at,
		       embedding <=> $1::vector AS distance
		FROM retrieval_chunks
		ORDER BY embedding <=> $1::vector
		LIMIT $2`,
		formatVector(queryEmbedding), topK,
	)
	if err != nil {
		return nil, fmt.Errorf("retrieval: search: %w", err)
	}
	defer rows.Close()

	var out []ScoredChunk
	for rows.Next() {
		var sc ScoredChunk
		var embText string
		if err := rows.Scan(&sc.ChunkID, &sc.DocID, &sc.ChunkIndex, &sc.Content, &embText,
			&sc.SourceDigest, &sc.InjectionScanStatus, &sc.CreatedAt, &sc.Distance); err != nil {
			return nil, fmt.Errorf("retrieval: scan search result: %w", err)
		}
		sc.TenantID = tenantID
		emb, err := parseVector(embText)
		if err != nil {
			return nil, err
		}
		sc.Embedding = emb
		out = append(out, sc)
	}
	return out, rows.Err()
}

// DeleteDocument hard-deletes one document and every chunk indexed under
// it (ON DELETE CASCADE, migrations/0022_retrieval.sql) — used by an
// operator re-ingesting a corrected version of a source file, distinct
// from DeleteTenant's erasure path below.
func DeleteDocument(ctx context.Context, tx pgx.Tx, tenantID, docID uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM documents WHERE tenant_id = $1 AND doc_id = $2`, tenantID, docID)
	if err != nil {
		return fmt.Errorf("retrieval: delete document %s: %w", docID, err)
	}
	return nil
}

// DeleteTenant hard-deletes every document (and, via ON DELETE CASCADE,
// every chunk) for tenantID — task 12.8's own erasure gate: "shredding a
// tenant's DEK makes its retrieval index unrecoverable too ... indexed
// chunks are a derived artifact, hard-deleted in the same erasure
// transaction." Called by the SAME caller that runs
// internal/crypto.EraseTenant, inside the SAME tx (cmd/nexusd's `erase
// --tenant` path) — this package deliberately does not import
// internal/crypto so the dependency runs the other way (a late,
// feature-scoped package depending on a Phase-1 foundation one, never the
// reverse).
func DeleteTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (deletedChunks, deletedDocs int, err error) {
	chunkTag, err := tx.Exec(ctx, `DELETE FROM retrieval_chunks WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return 0, 0, fmt.Errorf("retrieval: erase tenant %s chunks: %w", tenantID, err)
	}
	docTag, err := tx.Exec(ctx, `DELETE FROM documents WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return 0, 0, fmt.Errorf("retrieval: erase tenant %s documents: %w", tenantID, err)
	}
	return int(chunkTag.RowsAffected()), int(docTag.RowsAffected()), nil
}
