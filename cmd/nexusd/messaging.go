package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/email"
)

// MessagingChannels backs internal/surfaces/telegram.ChannelPort,
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
type MessagingChannels struct {
	Store *store.Store
	Keys  *crypto.KeyStore
}

type channelRow struct {
	SealedCredential []byte
	KeyID            string
	WebhookSecret    string
	Config           map[string]string
	Status           string
}

func (m *MessagingChannels) load(ctx context.Context, tenantID uuid.UUID, kind string) (channelRow, bool, error) {
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
		if err == pgx.ErrNoRows {
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

func (m *MessagingChannels) unsealCredential(ctx context.Context, tenantID uuid.UUID, kind string, row channelRow) (string, error) {
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

func (m *MessagingChannels) WebhookSecret(ctx context.Context, tenantID uuid.UUID) (string, bool, error) {
	row, ok, err := m.load(ctx, tenantID, "telegram")
	if err != nil || !ok {
		return "", ok, err
	}
	return row.WebhookSecret, true, nil
}

func (m *MessagingChannels) BotToken(ctx context.Context, tenantID uuid.UUID) (string, error) {
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

func (m *MessagingChannels) AppSecret(ctx context.Context, tenantID uuid.UUID) (string, bool, error) {
	row, ok, err := m.load(ctx, tenantID, "zalo")
	if err != nil || !ok {
		return "", ok, err
	}
	return row.WebhookSecret, true, nil
}

func (m *MessagingChannels) AccessToken(ctx context.Context, tenantID uuid.UUID) (string, error) {
	row, ok, err := m.load(ctx, tenantID, "zalo")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no active zalo channel configured for tenant %s", tenantID)
	}
	return m.unsealCredential(ctx, tenantID, "zalo", row)
}

// --- email.ChannelPort ---

func (m *MessagingChannels) WebhookCredential(ctx context.Context, tenantID uuid.UUID) (string, string, bool, error) {
	row, ok, err := m.load(ctx, tenantID, "email_smtp")
	if err != nil || !ok {
		return "", "", ok, err
	}
	return row.Config["webhook_username"], row.WebhookSecret, true, nil
}

func (m *MessagingChannels) SMTPConfig(ctx context.Context, tenantID uuid.UUID) (email.SMTPConfig, error) {
	row, ok, err := m.load(ctx, tenantID, "email_smtp")
	if err != nil {
		return email.SMTPConfig{}, err
	}
	if !ok {
		return email.SMTPConfig{}, fmt.Errorf("no active email channel configured for tenant %s", tenantID)
	}
	password, err := m.unsealCredential(ctx, tenantID, "email_smtp", row)
	if err != nil {
		return email.SMTPConfig{}, err
	}
	port := 587
	if p := row.Config["smtp_port"]; p != "" {
		_, _ = fmt.Sscanf(p, "%d", &port)
	}
	return email.SMTPConfig{
		Host:        row.Config["smtp_host"],
		Port:        port,
		Username:    row.Config["smtp_username"],
		Password:    password,
		FromAddress: row.Config["from_address"],
	}, nil
}
