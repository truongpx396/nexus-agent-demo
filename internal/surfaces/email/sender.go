package email

import (
	"context"
	"encoding/json"
	"fmt"
	"net/smtp"

	"github.com/google/uuid"
)

// notificationPayload mirrors internal/surfaces/telegram's own (its doc
// comment) — the explicit, discriminated shape drainAndNotify (webhook.go)
// builds and Send below interprets.
type notificationPayload struct {
	Kind      string `json:"kind"` // "approval" | "content"
	SessionID string `json:"session_id,omitempty"`
	ToolID    string `json:"tool_id,omitempty"`
	Text      string `json:"text,omitempty"`
}

// Sender implements surfaces.Sender via stdlib net/smtp — no external SMTP
// library dependency, matching this codebase's minimal-dependency style.
// SMTP has no context-cancellation hook of its own; ctx is accepted (the
// interface requires it) but net/smtp.SendMail runs to completion or
// error, uninterruptible mid-call — the same honest limitation any
// stdlib-only SMTP client has.
type Sender struct {
	Channels ChannelPort
	TenantID uuid.UUID
}

// Send implements surfaces.Sender. recipient is the original sender's own
// address (webhook.go's own recipientAddress) — a reply goes back to
// whoever wrote in.
func (s *Sender) Send(ctx context.Context, surfaceID, recipient string, payload []byte) error {
	cfg, err := s.Channels.SMTPConfig(ctx, s.TenantID)
	if err != nil {
		return fmt.Errorf("email: resolve SMTP config: %w", err)
	}

	subject, text := renderNotification(payload)
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s\r\n", cfg.FromAddress, recipient, subject, text)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	if err := smtp.SendMail(addr, auth, cfg.FromAddress, []string{recipient}, []byte(msg)); err != nil {
		return fmt.Errorf("email: send: %w", err)
	}
	return nil
}

// renderNotification is Send's own pure discriminator — factored out so it
// can be tested directly: net/smtp.SendMail dials a real connection with
// no client-injection point (unlike telegram/zalo's http.Client field), so
// this is the one piece of Send's logic this package CAN unit-test without
// a live SMTP server.
func renderNotification(payload []byte) (subject, text string) {
	text = string(payload)
	subject = "Approval needed"
	var body notificationPayload
	if err := json.Unmarshal(payload, &body); err == nil {
		switch body.Kind {
		case "approval":
			text = fmt.Sprintf("Approval needed for session %s (tool: %s) — review it in the run's own approval endpoint.", body.SessionID, body.ToolID)
		case "content":
			text = body.Text
			subject = "Re: your message"
		}
	}
	return subject, text
}
