// Package email is a webhook surface (README Phase 11, task 11.5): inbound
// via a provider webhook (IMAP poll is a documented deferral — no stdlib
// IMAP client and a poller is meaningfully more code for no additional
// pattern coverage this phase needs), outbound through stdlib net/smtp.
// Structurally the same shape as internal/surfaces/telegram/zalo (their own
// doc comments) — a per-turn principal resolved fresh from the inbound
// payload, session-creation-then-StartRun, outbox-delivered approval
// notifications. Zero kernel change.
package email

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
)

// emailNamespace is telegram.telegramNamespace's own counterpart — a fixed
// UUID namespace for deriving a stable per-(tenant,from-address) UserID,
// recomputed fresh from every inbound message rather than stored in a
// mapping row (task 7.13).
var emailNamespace = uuid.MustParse("6f8e1a2b-0000-4000-8000-000000000003")

// inboundMessage is the one concrete parsed-inbound-email JSON shape this
// surface accepts — the field set common to inbound-parse webhooks (a
// Postmark/Mailgun/SendGrid-style provider adapter would translate its own
// wire format into this before POSTing here, or POST it directly if it
// already matches).
type inboundMessage struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	TextBody  string `json:"text_body"`
	MessageID string `json:"message_id"`
}

// ChannelPort resolves the tenant's admitted email credentials — inbound
// Basic Auth for the webhook, outbound SMTP for replies — both backed by
// migrations/0021_messaging_channels.sql (kind='email_smtp').
type ChannelPort interface {
	// WebhookCredential returns the Basic Auth (username, password) this
	// tenant's inbound webhook URL is protected by. ok=false means no
	// active email channel is configured for tenantID at all.
	WebhookCredential(ctx context.Context, tenantID uuid.UUID) (username, password string, ok bool, err error)
	// SMTPConfig returns everything Sender needs to submit one outbound
	// message on tenantID's behalf — password unsealed, used only inside
	// Sender.Send.
	SMTPConfig(ctx context.Context, tenantID uuid.UUID) (SMTPConfig, error)
}

// SMTPConfig is one tenant's outbound submission config.
type SMTPConfig struct {
	Host        string
	Port        int
	Username    string
	Password    string
	FromAddress string
}

// Server holds everything one email webhook handler needs.
type Server struct {
	Store                 *store.Store
	KeyStore              *crypto.KeyStore
	Starter               RunStarter
	Channels              ChannelPort
	CatalogManifestDigest []byte

	// Outbox, if set, backs durable at-least-once delivery of
	// EventApprovalRequested — see internal/surfaces/telegram.Server's own
	// doc comment on why a *Sender is constructed per-delivery.
	Outbox    *surfaces.Outbox
	RateLimit *RateLimiter
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/webhooks/email/{tenant_id}", s.handleWebhook)
	return mux
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.PathValue("tenant_id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}

	wantUser, wantPass, ok, err := s.Channels.WebhookCredential(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "resolve channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no email channel configured for this tenant", http.StatusNotFound)
		return
	}
	// Basic Auth verified BEFORE the body is ever read/parsed
	// (docs/constitution.md: "before the kernel sees the payload").
	gotUser, gotPass, hasAuth := r.BasicAuth()
	userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(wantUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(wantPass)) == 1
	if !hasAuth || !userOK || !passOK {
		w.Header().Set("WWW-Authenticate", `Basic realm="nexus-email-webhook"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.RateLimit != nil && !s.RateLimit.Allow(tenantID.String()) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var msg inboundMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "invalid message", http.StatusBadRequest)
		return
	}
	if msg.From == "" || msg.TextBody == "" {
		http.Error(w, "from and text_body are required", http.StatusBadRequest)
		return
	}

	userID := uuid.NewSHA1(emailNamespace, fmt.Appendf(nil, "%s:%s", tenantID, msg.From))
	input := msg.TextBody
	if msg.Subject != "" {
		input = msg.Subject + "\n\n" + msg.TextBody
	}

	if _, err := s.startRun(r.Context(), tenantID, userID, msg.From, input); err != nil {
		slog.Error("email: start run", "error", err, "tenant_id", tenantID)
		http.Error(w, "start run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// startRun mirrors every other surface's own (their doc comments) —
// duplicated per this codebase's established cross-surface idiom.
// recipientAddress is the inbound sender's own address, used as this run's
// outbox delivery recipient for any reply.
func (s *Server) startRun(ctx context.Context, tenantID, userID uuid.UUID, recipientAddress, input string) (uuid.UUID, error) {
	route := provider.Route(provider.DataLabelInternal, provider.DifficultySimple)
	sessionID := uuid.New()
	digest := harness.Digest(harness.Config{
		SystemPromptVersion:   "phase2-v1",
		CatalogManifestDigest: s.CatalogManifestDigest,
		PromptMode:            "phase2-single-shot",
	})

	var dek crypto.DEK
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = s.KeyStore.NewDEK(ctx, tx, tenantID)
		if derr != nil {
			return derr
		}
		return store.CreateSession(ctx, tx, store.Session{
			SessionID:     sessionID,
			SessionKey:    sessionID.String(),
			TenantID:      tenantID,
			SurfaceID:     "email",
			UserID:        userID,
			AgentVersion:  1,
			HarnessDigest: digest,
			DataLabel:     string(provider.DataLabelInternal),
			RouteModelID:  route.ModelID,
			RouteReason:   route.Reason,
			AutonomyLevel: "supervised",
		})
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create session: %w", err)
	}

	req := RunRequest{
		SessionID: sessionID, TenantID: tenantID,
		Seal:          sealFuncFor(dek, tenantID, sessionID),
		Input:         input,
		ModelID:       route.ModelID,
		AutonomyLevel: "supervised",
	}
	events, err := s.Starter.StartRun(context.Background(), req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("start run: %w", err)
	}
	go s.drainAndNotify(tenantID, sessionID, recipientAddress, events)
	return sessionID, nil
}

func (s *Server) drainAndNotify(tenantID, sessionID uuid.UUID, recipientAddress string, events <-chan RunEvent) {
	if s.Outbox == nil {
		for range events {
		}
		return
	}
	sender := &Sender{Channels: s.Channels, TenantID: tenantID}
	for re := range events {
		if re.Err != nil || re.Event.Type != store.EventApprovalRequested {
			continue
		}
		toolID := ""
		if re.Event.ToolID != nil {
			toolID = *re.Event.ToolID
		}
		payload, err := json.Marshal(map[string]string{"session_id": sessionID.String(), "tool_id": toolID})
		if err != nil {
			continue
		}
		if err := s.Outbox.Deliver(context.Background(), tenantID, sessionID, re.Event.Seq, "email", recipientAddress, payload, sender); err != nil {
			slog.Error("email: deliver approval notification", "error", err, "session_id", sessionID)
		}
	}
}

func sealFuncFor(dek crypto.DEK, tenantID, sessionID uuid.UUID) SealFunc {
	return func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("seal event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}
}
