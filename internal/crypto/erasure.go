package crypto

import (
	"errors"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// ErrKeyShredded is returned when the requested key has been crypto-shredded
// (Phase 5): the row still exists so the audit chain and any digest
// referencing it remain intact, but the key material is gone and can never
// be recovered.
var ErrKeyShredded = errors.New("encryption key has been shredded")

// ErasureKeyID is the sentinel Event.KeyID an EventErasure record carries.
// Erasure metadata (who/why/when) is not tenant-secret content the way an
// ordinary event's payload is — it is a structural, administrative fact,
// same spirit as internal/obs's "structure, not content" — so it is stored
// as PLAINTEXT directly in Payload rather than sealed under any DEK
// (there's no DEK left to seal it under once the tenant's key is shredded
// anyway). ErasureKeyID never names a real encryption_keys row; a reader
// checks for it before attempting KeyStore.Unwrap.
const ErasureKeyID = "erasure-record"

// DerivedArtifact mirrors one derived_artifacts row (migrations/
// 0008_derived_artifacts.sql) — a plaintext-derived file living outside the
// encrypted event log (today, only internal/tools.BlobStore's oversized-
// result spill files).
type DerivedArtifact struct {
	ArtifactID uuid.UUID
	TenantID   uuid.UUID
	SessionID  uuid.UUID
	Kind       string
	Path       string
}

// ErasureResult reports what one erasure transaction did. Callers are
// responsible for two post-commit, best-effort steps that cannot themselves
// be transactional with Postgres: unlinking DeletedArtifacts' Path values,
// and (for a caller that streams events to a client, e.g. an admin surface)
// forwarding ErasureEvents. ReconcileDerivedArtifacts is the backstop for
// whatever this best-effort step misses.
type ErasureResult struct {
	ShreddedKeyIDs   []string
	DeletedArtifacts []DerivedArtifact
	ErasureEvents    []eventlog.Event
}
