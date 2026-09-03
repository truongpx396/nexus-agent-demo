-- Retrieval tier (README Phase 12, tasks 12.1-12.3): pgvector lands here,
-- on the trigger §8 names ("file-first memory is actually exhausted"), not
-- pre-built speculatively in Phase 1 — the infrastructure-collapse table
-- (README.md §2) explicitly deferred pgvector out of the core 10 phases.
--
-- Two tables, mirroring the ingest/index split (internal/ingest vs
-- internal/retrieval):
--   documents        — one row per admitted source file; the admission-scan
--                       gate (internal/ingest/admit.go, task 12.2) fails
--                       closed the same way a skill bundle's own
--                       pending/clean/flagged/rejected gate does
--                       (migrations/0008's own cross-reference); only a
--                       'clean' document is ever chunked into
--                       retrieval_chunks.
--   retrieval_chunks — the pgvector index itself: (tenant_id, doc_id,
--                       chunk_id, embedding, source_digest) verbatim from
--                       README.md §4's schema note, tenant-scoped and
--                       RLS-enabled like every table in this system (task
--                       12.3's "no exception for embeddings").
--
-- Neither table is session-scoped: a document is ingested once per tenant
-- and read across many sessions, the same "outlives any one run" shape
-- oauth_connections (migrations/0018) already established for a different
-- durable, tenant-owned resource. Erasure (task 12.8) therefore hangs off
-- tenant-wide erasure only (internal/crypto.EraseTenant's caller,
-- cmd/nexusd's own `erase --tenant` path, deletes both tables in the SAME
-- transaction as the DEK shred) — there is no per-session retrieval state
-- for `erase --session` to reach.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE documents (
    doc_id           uuid PRIMARY KEY,
    tenant_id          uuid NOT NULL REFERENCES tenants (tenant_id),
    source_name          text NOT NULL,
    mime_type              text NOT NULL,
    source_digest            bytea NOT NULL,  -- sha256 over the raw uploaded bytes (internal/crypto.Digest) — task 12.1's "deterministic per-document digest"
    chunk_count                int NOT NULL DEFAULT 0,
    admission_status              text NOT NULL DEFAULT 'pending',
    admission_findings              jsonb NOT NULL DEFAULT '[]',
    created_at                        timestamptz NOT NULL DEFAULT now(),

    CHECK (admission_status IN ('pending', 'clean', 'flagged', 'rejected'))
);

CREATE INDEX documents_tenant_idx ON documents (tenant_id);

ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents FORCE ROW LEVEL SECURITY;

CREATE POLICY documents_isolation ON documents
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- embedding's fixed width (32) matches internal/retrieval.EmbeddingDimensions
-- exactly — internal/provider/fake's deterministic embedding fake (task
-- 12.5) is this demo's ONLY embedder, and 32 is its documented, fixed
-- output width. A real embedding adapter would carry its own native
-- dimension (1536, 3072, ...); swapping one in is an ALTER COLUMN TYPE
-- migration, the same expand/contract discipline every other schema change
-- in this system already follows (migrations/README.md).
CREATE TABLE retrieval_chunks (
    chunk_id           uuid PRIMARY KEY,
    tenant_id            uuid NOT NULL REFERENCES tenants (tenant_id),
    doc_id                 uuid NOT NULL REFERENCES documents (doc_id) ON DELETE CASCADE,
    chunk_index              int NOT NULL,
    content                    text NOT NULL,
    embedding                    vector(32) NOT NULL,
    source_digest                  bytea NOT NULL,  -- copied from documents.source_digest — task 12.1's "reuses the digest-over-plaintext idea from #32" carried onto the row a search result actually returns
    injection_scan_status              text NOT NULL DEFAULT 'clean',
    created_at                            timestamptz NOT NULL DEFAULT now(),

    UNIQUE (doc_id, chunk_index),
    CHECK (injection_scan_status IN ('clean', 'flagged', 'rejected'))
);

CREATE INDEX retrieval_chunks_tenant_idx ON retrieval_chunks (tenant_id);
CREATE INDEX retrieval_chunks_doc_idx ON retrieval_chunks (doc_id);
-- IVFFlat needs at least a handful of rows to train against; a demo-scale
-- corpus is happy with an exact scan, so no index is created on `embedding`
-- itself. Adding `USING ivfflat (embedding vector_cosine_ops)` once a real
-- corpus exists is an additive, non-breaking migration — the query shape
-- (ORDER BY embedding <=> $1) never changes.

ALTER TABLE retrieval_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE retrieval_chunks FORCE ROW LEVEL SECURITY;

CREATE POLICY retrieval_chunks_isolation ON retrieval_chunks
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
