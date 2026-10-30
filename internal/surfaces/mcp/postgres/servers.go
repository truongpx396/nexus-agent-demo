// Package postgres is the Postgres adapter for mcp.ServerDirectory: the
// tenant's registered MCP servers and, for a bearer_static server, the
// sealed token that authenticates to it.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/mcp"
)

// Directory implements mcp.ServerDirectory over mcp_servers.
type Directory struct {
	st   *store.Store
	keys *crypto.KeyStore
}

var _ mcp.ServerDirectory = (*Directory)(nil)

// New returns a Directory; keys opens a bearer_static server's sealed token.
func New(st *store.Store, keys *crypto.KeyStore) *Directory { return &Directory{st: st, keys: keys} }

const selectServer = `SELECT server_id, name, base_url, auth_kind, key_id, oauth_provider, status FROM mcp_servers`

type scanner interface{ Scan(dest ...any) error }

func scanServer(row scanner) (mcp.Server, error) {
	var s mcp.Server
	var keyID, oauthProvider *string
	if err := row.Scan(&s.ServerID, &s.Name, &s.BaseURL, &s.AuthKind, &keyID, &oauthProvider, &s.Status); err != nil {
		return mcp.Server{}, err
	}
	if oauthProvider != nil {
		s.OAuthProvider = *oauthProvider
	}
	return s, nil
}

func (d *Directory) Admitted(ctx context.Context, tenantID uuid.UUID) ([]mcp.Server, error) {
	var out []mcp.Server
	err := d.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, selectServer+` WHERE tenant_id = $1 AND status = 'admitted' ORDER BY name`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanServer(rows)
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

func (d *Directory) Find(ctx context.Context, tenantID uuid.UUID, name string) (mcp.Server, bool, error) {
	var (
		srv   mcp.Server
		found bool
	)
	err := d.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, err := scanServer(tx.QueryRow(ctx, selectServer+` WHERE tenant_id = $1 AND name = $2`, tenantID, name))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		srv, found = s, true
		return nil
	})
	return srv, found, err
}

func (d *Directory) StaticToken(ctx context.Context, tenantID uuid.UUID, serverName string) (string, error) {
	var (
		sealed []byte
		keyID  *string
	)
	err := d.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT sealed_static_token, key_id FROM mcp_servers WHERE tenant_id = $1 AND name = $2`,
			tenantID, serverName,
		).Scan(&sealed, &keyID); err != nil {
			return fmt.Errorf("load sealed token for server %q: %w", serverName, err)
		}
		if keyID == nil || *keyID == "" {
			return fmt.Errorf("server %q declares bearer_static auth with no key_id", serverName)
		}
		dek, err := d.keys.Unwrap(ctx, tx, *keyID)
		if err != nil {
			return fmt.Errorf("unwrap static token key: %w", err)
		}
		aad := tenantID.String() + "|mcp|" + serverName
		raw, err := crypto.Open(dek, sealed, tenantID.String(), aad)
		if err != nil {
			return fmt.Errorf("open static token: %w", err)
		}
		sealed = raw
		return nil
	})
	if err != nil {
		return "", err
	}
	return string(sealed), nil
}

func (d *Directory) SessionUser(ctx context.Context, tenantID, sessionID uuid.UUID) (uuid.UUID, error) {
	var userID uuid.UUID
	err := d.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		sess, err := store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return fmt.Errorf("resolve calling session: %w", err)
		}
		userID = sess.UserID
		return nil
	})
	return userID, err
}
