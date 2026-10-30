package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/sealed"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// GrantStore implements obs.GrantStore over Postgres: the content_access_grants
// table, the audit events about it (sealed under the session's own key and
// hash-chained through Chain, in the same transaction as the change they
// record), and the decryption of a session's history.
type GrantStore struct {
	st     *store.Store
	keys   *cryptopg.KeyStore
	sealer *sealed.Sealer
}

var _ obs.GrantStore = (*GrantStore)(nil)

// NewGrantStore returns a GrantStore. chain may be nil, in which case audit
// events are appended without a signed receipt (a minimal wiring for tests).
func NewGrantStore(st *store.Store, keys *cryptopg.KeyStore, chain *auditpg.Chain) *GrantStore {
	return &GrantStore{st: st, keys: keys, sealer: sealed.NewSealer(keys, chain)}
}

func (s *GrantStore) Issue(ctx context.Context, gr obs.Grant) (obs.Grant, error) {
	err := s.st.InTenantTx(ctx, gr.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO content_access_grants (grant_id, tenant_id, session_id, grantee_id, reason, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at`,
			gr.GrantID, gr.TenantID, gr.SessionID, gr.GranteeID, gr.Reason, gr.ExpiresAt,
		).Scan(&gr.CreatedAt); err != nil {
			return fmt.Errorf("obs: insert content access grant: %w", err)
		}
		return s.appendEvent(ctx, tx, gr.TenantID, gr.SessionID, eventlog.EventContentAccessGranted, grantEventPayload{GrantID: gr.GrantID, GranteeID: gr.GranteeID, Reason: gr.Reason})
	})
	return gr, err
}

func (s *GrantStore) ListFor(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID) ([]obs.Grant, error) {
	var out []obs.Grant
	err := s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT grant_id, tenant_id, session_id, grantee_id, reason, expires_at, revoked_at, created_at
			FROM content_access_grants WHERE session_id = $1 AND grantee_id = $2 ORDER BY created_at DESC`,
			sessionID, granteeID,
		)
		if err != nil {
			return fmt.Errorf("obs: query content access grants: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var g obs.Grant
			if err := rows.Scan(&g.GrantID, &g.TenantID, &g.SessionID, &g.GranteeID, &g.Reason, &g.ExpiresAt, &g.RevokedAt, &g.CreatedAt); err != nil {
				return fmt.Errorf("obs: scan content access grant: %w", err)
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, err
}

func (s *GrantStore) RecordRefusal(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID, reason string) error {
	return s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.appendEvent(ctx, tx, tenantID, sessionID, eventlog.EventContentAccessRefused, refusedPayload{GranteeID: granteeID, Reason: reason})
	})
}

func (s *GrantStore) RecordAccess(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID, eventCount int) error {
	return s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.appendEvent(ctx, tx, tenantID, sessionID, eventlog.EventContentAccessed, accessedPayload{GranteeID: granteeID, EventCount: eventCount})
	})
}

func (s *GrantStore) History(ctx context.Context, tenantID, sessionID uuid.UUID) ([]obs.EventDTO, error) {
	var history []eventlog.Event
	err := s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		return nil, err
	}

	dekCache := map[string]crypto.DEK{}
	out := make([]obs.EventDTO, 0, len(history))
	for _, e := range history {
		dto := obs.EventDTO{EventID: e.EventID, Seq: e.Seq, Type: e.Type, CreatedAt: e.CreatedAt}
		if e.Type == eventlog.EventThought {
			out = append(out, dto) // never shown, even under a grant
			continue
		}
		if e.KeyID == crypto.ErasureKeyID {
			dto.Body = json.RawMessage(e.Payload) // stored plaintext already
			out = append(out, dto)
			continue
		}
		dek, ok := dekCache[e.KeyID]
		if !ok {
			if err := s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var uerr error
				dek, uerr = s.keys.Unwrap(ctx, tx, e.KeyID)
				return uerr
			}); err != nil {
				return nil, fmt.Errorf("obs: unwrap key for event %s: %w", e.EventID, err)
			}
			dekCache[e.KeyID] = dek
		}
		plaintext, err := crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, fmt.Errorf("obs: decrypt event %s: %w", e.EventID, err)
		}
		dto.Body = plaintext
		out = append(out, dto)
	}
	return out, nil
}

type grantEventPayload struct {
	GrantID   uuid.UUID `json:"grant_id"`
	GranteeID uuid.UUID `json:"grantee_id"`
	Reason    string    `json:"reason,omitempty"`
}

type refusedPayload struct {
	GranteeID uuid.UUID `json:"grantee_id"`
	Reason    string    `json:"reason,omitempty"`
}

type accessedPayload struct {
	GranteeID  uuid.UUID `json:"grantee_id"`
	EventCount int       `json:"event_count"`
}

// appendEvent appends one audit event about a grant, sealed under the
// session's own key and chained, in the caller's transaction.
func (s *GrantStore) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ eventlog.EventType, payload any) error {
	_, err := s.sealer.Bind(tx).Append(ctx, tenantID, sessionID, typ, nil, nil, payload)
	return err
}
