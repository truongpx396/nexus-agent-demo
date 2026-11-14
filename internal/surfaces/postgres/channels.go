package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent/internal/crypto"
	cryptopg "github.com/truongpx396/nexus-agent/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent/internal/store"
)

// Channels is the Postgres adapter behind internal/surfaces/telegram.ChannelPort,
// zalo.ChannelPort, and email.ChannelPort all at once — one Go type
// satisfying three structurally different interfaces simultaneously is
// ordinary Go, not a shared abstraction the three surface packages
// themselves depend on (each still declares its own interface, per this
// codebase's cross-surface decoupling idiom). All three read the SAME
// migrations/0021_messaging_channels.sql table, filtered by kind.
//
// The mapping this type settles on: webhook_secret is whatever verifies
// INBOUND authenticity (Telegram's secret_token, Zalo's HMAC key, email's
// Basic Auth password) — a plain column, never sealed, compared per
// request (that migration's own doc comment on why). sealed_credential is
// whatever authenticates OUTBOUND sends (Telegram bot token, Zalo OA
// access token, SMTP password) — envelope-encrypted under the tenant's
// own DEK, unsealed only inside a Sender's own Send call.
type Channels struct {
	Store *store.Store
	Keys  *cryptopg.KeyStore
}

// NewChannels returns the channel directory over st, unsealing outbound
// credentials with keys.
func NewChannels(st *store.Store, keys *cryptopg.KeyStore) *Channels {
	return &Channels{Store: st, Keys: keys}
}

type channelRow struct {
	SealedCredential []byte
	KeyID            string
	WebhookSecret    string
	Config           map[string]string
	Status           string
}

func (m *Channels) load(ctx context.Context, tenantID uuid.UUID, kind string) (channelRow, bool, error) {
	var row channelRow
	var rawConfig []byte
	var keyID *string
	var webhookSecret *string
	err := m.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT sealed_credential, key_id, webhook_secret, config, status FROM messaging_channels WHERE tenant_id = $1 AND kind = $2`,
			tenantID, kind,
		).Scan(&row.SealedCredential, &keyID, &webhookSecret, &rawConfig, &row.Status)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return channelRow{}, false, nil
		}
		return channelRow{}, false, err
	}
	if keyID != nil {
		row.KeyID = *keyID
	}
	if webhookSecret != nil {
		row.WebhookSecret = *webhookSecret
	}
	if len(rawConfig) > 0 {
		row.Config = map[string]string{}
		_ = json.Unmarshal(rawConfig, &row.Config)
	}
	if row.Status != "active" {
		return channelRow{}, false, nil
	}
	return row, true, nil
}

func (m *Channels) unsealCredential(ctx context.Context, tenantID uuid.UUID, kind string, row channelRow) (string, error) {
	var plaintext []byte
	err := m.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		dek, err := m.Keys.Unwrap(ctx, tx, row.KeyID)
		if err != nil {
			return err
		}
		aad := tenantID.String() + "|" + kind
		plaintext, err = crypto.Open(dek, row.SealedCredential, tenantID.String(), aad)
		return err
	})
	return string(plaintext), err
}

// --- telegram.ChannelPort ---

func (m *Channels) WebhookSecret(ctx context.Context, tenantID uuid.UUID) (string, bool, error) {
	row, ok, err := m.load(ctx, tenantID, "telegram")
	if err != nil || !ok {
		return "", ok, err
	}
	return row.WebhookSecret, true, nil
}

func (m *Channels) BotToken(ctx context.Context, tenantID uuid.UUID) (string, error) {
	row, ok, err := m.load(ctx, tenantID, "telegram")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no active telegram channel configured for tenant %s", tenantID)
	}
	return m.unsealCredential(ctx, tenantID, "telegram", row)
}

// --- zalo.ChannelPort ---

func (m *Channels) AppSecret(ctx context.Context, tenantID uuid.UUID) (string, bool, error) {
	row, ok, err := m.load(ctx, tenantID, "zalo")
	if err != nil || !ok {
		return "", ok, err
	}
	return row.WebhookSecret, true, nil
}

func (m *Channels) AccessToken(ctx context.Context, tenantID uuid.UUID) (string, error) {
	row, ok, err := m.load(ctx, tenantID, "zalo")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no active zalo channel configured for tenant %s", tenantID)
	}
	return m.unsealCredential(ctx, tenantID, "zalo", row)
}

// SMTPSettings is one tenant's outbound mail submission config. Its fields match
// the email surface's own SMTPConfig one for one, on purpose: this adapter is
// shared by every chat surface and so must not import any one of them, and
// cmd/nexusd satisfies email.ChannelPort by converting between the two types.
type SMTPSettings struct {
	Host        string
	Port        int
	Username    string
	Password    string
	FromAddress string
}

// --- email.ChannelPort (see SMTPSettings for the one method that needs a conversion) ---

func (m *Channels) WebhookCredential(ctx context.Context, tenantID uuid.UUID) (string, string, bool, error) {
	row, ok, err := m.load(ctx, tenantID, "email_smtp")
	if err != nil || !ok {
		return "", "", ok, err
	}
	return row.Config["webhook_username"], row.WebhookSecret, true, nil
}

func (m *Channels) SMTPConfig(ctx context.Context, tenantID uuid.UUID) (SMTPSettings, error) {
	row, ok, err := m.load(ctx, tenantID, "email_smtp")
	if err != nil {
		return SMTPSettings{}, err
	}
	if !ok {
		return SMTPSettings{}, fmt.Errorf("no active email channel configured for tenant %s", tenantID)
	}
	password, err := m.unsealCredential(ctx, tenantID, "email_smtp", row)
	if err != nil {
		return SMTPSettings{}, err
	}
	port := 587
	if p := row.Config["smtp_port"]; p != "" {
		_, _ = fmt.Sscanf(p, "%d", &port)
	}
	return SMTPSettings{
		Host:        row.Config["smtp_host"],
		Port:        port,
		Username:    row.Config["smtp_username"],
		Password:    password,
		FromAddress: row.Config["from_address"],
	}, nil
}
