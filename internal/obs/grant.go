package obs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Grant mirrors one content_access_grants row (migrations/
// 0009_content_access_grants.sql): the only path to plaintext outside a
// run's own audience (README task 5.11, pattern 35). This is deliberately
// distinct from internal/surfaces/rest/server.go's existing owner-decrypt
// path (handleEvents, gated on sess.UserID == userID — pattern #51,
// already shipped): that path is the run's OWN audience reading its own
// content; a Grant authorizes someone ELSE — an operator/support
// principal — for a bounded, audited, expiring window.
type Grant struct {
	GrantID   uuid.UUID
	TenantID  uuid.UUID
	SessionID uuid.UUID
	GranteeID uuid.UUID
	Reason    string
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

func (g Grant) valid(at time.Time) bool {
	return g.RevokedAt == nil && at.Before(g.ExpiresAt)
}

// Grants issues and enforces content access grants. Every grant AND every
// read under it produces its own hash-chained audit receipt
// (content_access_granted, content_accessed) — "audited, expiring, receipt
// per read" (task 5.11's own wording).
type Grants struct {
	Store *store.Store
	Keys  *crypto.KeyStore
	Chain *audit.Chain
}

func NewGrants(st *store.Store, keys *crypto.KeyStore, chain *audit.Chain) *Grants {
	return &Grants{Store: st, Keys: keys, Chain: chain}
}

// RequestGrant issues a new grant and appends EventContentAccessGranted.
func (g *Grants) RequestGrant(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID, reason string, ttl time.Duration) (Grant, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	gr := Grant{GrantID: uuid.New(), TenantID: tenantID, SessionID: sessionID, GranteeID: granteeID, Reason: reason, ExpiresAt: time.Now().Add(ttl)}
	err := g.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO content_access_grants (grant_id, tenant_id, session_id, grantee_id, reason, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at`,
			gr.GrantID, gr.TenantID, gr.SessionID, gr.GranteeID, gr.Reason, gr.ExpiresAt,
		).Scan(&gr.CreatedAt); err != nil {
			return fmt.Errorf("obs: insert content access grant: %w", err)
		}
		return g.appendEvent(ctx, tx, tenantID, sessionID, store.EventContentAccessGranted, grantEventPayload{GrantID: gr.GrantID, GranteeID: granteeID, Reason: reason})
	})
	return gr, err
}

// Authorize finds a currently-valid grant for (sessionID, granteeID) —
// unexpired and unrevoked. Refuses (and appends EventContentAccessRefused,
// its own audit trail for a DENIED attempt, not just a granted one) when
// none exists.
func (g *Grants) Authorize(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID) (Grant, error) {
	var gr Grant
	var found bool
	err := g.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT grant_id, tenant_id, session_id, grantee_id, reason, expires_at, revoked_at, created_at
			FROM content_access_grants WHERE session_id = $1 AND grantee_id = $2 ORDER BY created_at DESC`,
			sessionID, granteeID,
		)
		if err != nil {
			return fmt.Errorf("obs: query content access grants: %w", err)
		}
		now := time.Now()
		for rows.Next() {
			var cand Grant
			if serr := rows.Scan(&cand.GrantID, &cand.TenantID, &cand.SessionID, &cand.GranteeID, &cand.Reason, &cand.ExpiresAt, &cand.RevokedAt, &cand.CreatedAt); serr != nil {
				rows.Close()
				return fmt.Errorf("obs: scan content access grant: %w", serr)
			}
			if !found && cand.valid(now) {
				gr, found = cand, true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		if !found {
			return g.appendEvent(ctx, tx, tenantID, sessionID, store.EventContentAccessRefused, refusedPayload{GranteeID: granteeID, Reason: "no valid grant"})
		}
		return nil
	})
	if err != nil {
		return Grant{}, err
	}
	if !found {
		return Grant{}, fmt.Errorf("obs: no valid content access grant for session %s, grantee %s", sessionID, granteeID)
	}
	return gr, nil
}

// EventDTO is the decrypted shape Read returns — deliberately narrow (no
// Payload, no crypto details), the same information REST's own eventDTO
// exposes to a run's own audience.
type EventDTO struct {
	EventID   uuid.UUID
	Seq       int64
	Type      store.EventType
	Body      json.RawMessage
	CreatedAt time.Time
}

// Read authorizes granteeID against sessionID (Authorize) and, only on
// success, decrypts and returns every event's plaintext — recording ONE
// EventContentAccessed receipt for this read (task 5.11: "receipt ... on
// every read," read here meaning every access this method grants, not one
// receipt per individual event it happens to decrypt).
func (g *Grants) Read(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID) ([]EventDTO, error) {
	if _, err := g.Authorize(ctx, tenantID, sessionID, granteeID); err != nil {
		return nil, err
	}

	var history []store.Event
	err := g.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		return nil, err
	}

	dekCache := map[string]crypto.DEK{}
	out := make([]EventDTO, 0, len(history))
	for _, e := range history {
		dto := EventDTO{EventID: e.EventID, Seq: e.Seq, Type: e.Type, CreatedAt: e.CreatedAt}
		if e.Type == store.EventThought {
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
			if err := g.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var uerr error
				dek, uerr = g.Keys.Unwrap(ctx, tx, e.KeyID)
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

	err = g.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return g.appendEvent(ctx, tx, tenantID, sessionID, store.EventContentAccessed, accessedPayload{GranteeID: granteeID, EventCount: len(history)})
	})
	if err != nil {
		return nil, fmt.Errorf("obs: record content access receipt: %w", err)
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

// appendEvent seals payload under sessionID's active key (its most recent
// event whose KeyID isn't the erasure sentinel) and durably appends it,
// chaining it via Chain if set — the same shape internal/oversight's own
// deps.appendEvent uses, reimplemented here rather than shared: the two
// packages have no reason to depend on each other, and this is a handful of
// lines, not shared logic worth a common package for (the same call this
// codebase already makes for internal/hooks's and internal/tools/
// builtin's duplicated SSRF guards).
func (g *Grants) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ store.EventType, payload any) error {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("obs: marshal %s payload: %w", typ, err)
	}
	var keyID string
	if err := tx.QueryRow(ctx, `SELECT key_id FROM events WHERE session_id = $1 AND key_id != $2 ORDER BY seq DESC LIMIT 1`, sessionID, crypto.ErasureKeyID).Scan(&keyID); err != nil {
		return fmt.Errorf("obs: find active key for session %s: %w", sessionID, err)
	}
	dek, err := g.Keys.Unwrap(ctx, tx, keyID)
	if err != nil {
		return fmt.Errorf("obs: unwrap key for session %s: %w", sessionID, err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		return fmt.Errorf("obs: seal %s payload: %w", typ, err)
	}
	e := store.Event{
		EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, SchemaVersion: store.CurrentSchemaVersion,
		Type: typ, Payload: sealed, PayloadDigest: crypto.Digest(plaintext), KeyID: dek.KeyID, Actor: store.ActorSystem,
	}
	out, err := store.Append(ctx, tx, e)
	if err != nil {
		return fmt.Errorf("obs: append %s: %w", typ, err)
	}
	if g.Chain != nil {
		if _, err := g.Chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return fmt.Errorf("obs: chain %s: %w", typ, err)
		}
	}
	return nil
}
