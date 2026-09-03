// Package telegram is a webhook surface (README Phase 11, task 11.4):
// inbound Telegram Bot API updates resolve a per-turn principal and submit
// a run through the ordinary session-creation-then-StartRun sequence every
// surface uses; outbound replies go through the shared
// internal/surfaces.Outbox exactly like REST's own approval-notification
// path already does. Zero kernel change — this package never imports
// kernel/ (tests/contract/boundaries_test.go's wildcard rule).
package telegram

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

// telegramNamespace is a fixed, arbitrary UUID used only as the namespace
// for deriving a stable per-(tenant-bot,from.id) UserID (uuid.NewSHA1) —
// task 7.13's "resolved fresh from THIS request, never cached" is satisfied
// by recomputing this from the inbound payload on every update, not by
// storing a mapping row; the same Telegram user always maps to the same
// UUID without a lookup table.
var telegramNamespace = uuid.MustParse("6f8e1a2b-0000-4000-8000-000000000001")

// update is the minimal subset of Telegram's Bot API Update object this
// surface actually reads.
type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64 `json:"message_id"`
		From      struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

// ChannelPort resolves the tenant's admitted Telegram credentials —
// internal/connectors-shaped seam (structural, not imported) into
// migrations/0021_messaging_channels.sql; cmd/nexusd's own implementation
// reads the sealed bot token/webhook secret via internal/crypto.
type ChannelPort interface {
	// WebhookSecret returns the tenant's configured secret_token for
	// verifying X-Telegram-Bot-Api-Secret-Token — ok=false means no
	// active telegram channel is configured for tenantID at all.
	WebhookSecret(ctx context.Context, tenantID uuid.UUID) (secret string, ok bool, err error)
	// BotToken returns the tenant's sealed bot token, unsealed — used only
	// by Sender.Send, never logged or placed in an event payload.
	BotToken(ctx context.Context, tenantID uuid.UUID) (token string, err error)
}

// Server holds everything one Telegram webhook handler needs.
type Server struct {
	Store                 *store.Store
	KeyStore              *crypto.KeyStore
	Starter               RunStarter
	Channels              ChannelPort
	CatalogManifestDigest []byte

	// Outbox, if set, backs durable at-least-once delivery of
	// EventApprovalRequested (README task 7.14, reused unchanged) — nil
	// leaves it unmounted. Unlike REST's own OutboxSender (a single
	// stateless field: its dev-mode slogSender needs no per-tenant
	// credential), a real Sender here needs THIS delivery's own tenant's
	// bot token, so drainAndNotify constructs a *Sender per call instead
	// of reusing one fixed instance — surfaces.Sender's own interface
	// carries no tenant parameter to thread one through otherwise.
	Outbox     *surfaces.Outbox
	HTTPClient *http.Client

	RateLimit *RateLimiter
}

// Handler returns the http.Handler cmd/nexusd mounts at
// POST /v1/webhooks/telegram/{tenant_id} — tenant_id in the path is a
// routing key, not a credential; the actual authenticity check is the
// secret_token header compared below, BEFORE the body is ever parsed
// (docs/constitution.md: "verify provider authenticity ... before the
// kernel sees the payload").
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/webhooks/telegram/{tenant_id}", s.handleWebhook)
	return mux
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.PathValue("tenant_id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}

	secret, ok, err := s.Channels.WebhookSecret(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "resolve channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no telegram channel configured for this tenant", http.StatusNotFound)
		return
	}
	got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Rate-limit per external identity BEFORE the body is parsed — a
	// per-tenant bucket is the identity available at this point (Telegram's
	// per-chat identity is inside the body, which fail-closed auth above
	// has already gated); this still bounds a single compromised/abusive
	// tenant's webhook traffic.
	if s.RateLimit != nil && !s.RateLimit.Allow(tenantID.String()) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var upd update
	if err := json.Unmarshal(body, &upd); err != nil {
		http.Error(w, "invalid update", http.StatusBadRequest)
		return
	}
	if upd.Message == nil || upd.Message.Text == "" {
		w.WriteHeader(http.StatusOK) // not every update is a text message worth starting a run over; ack and ignore
		return
	}

	// Task 7.13: resolved fresh from THIS update, never cached.
	userID := uuid.NewSHA1(telegramNamespace, fmt.Appendf(nil, "%s:%d", tenantID, upd.Message.From.ID))
	chatID := fmt.Sprintf("%d", upd.Message.Chat.ID)

	if _, err := s.startRun(r.Context(), tenantID, userID, chatID, upd.Message.Text); err != nil {
		slog.Error("telegram: start run", "error", err, "tenant_id", tenantID)
		http.Error(w, "start run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// startRun mirrors internal/surfaces/rest's handleCreateRun (session + DEK
// creation, then RunStarter.StartRun) — duplicated per this codebase's
// established cross-surface idiom rather than imported, since REST and
// Telegram share no direct dependency. chatID is the Telegram delivery
// target for any outbox notification this run produces — carried alongside
// (never derived from) sessionID, since the two identify different things
// (surfaces.Outbox.Deliver's own recipient parameter).
func (s *Server) startRun(ctx context.Context, tenantID, userID uuid.UUID, chatID, input string) (uuid.UUID, error) {
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
			SurfaceID:     "telegram",
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
	events, err := s.Starter.StartRun(context.Background(), req) // a run outlives this webhook request
	if err != nil {
		return uuid.Nil, fmt.Errorf("start run: %w", err)
	}
	go s.drainAndNotify(tenantID, sessionID, chatID, events)
	return sessionID, nil
}

// drainAndNotify is publishUntilDone's Telegram-side counterpart
// (internal/surfaces/rest/server.go): the only consumer of StartRun's
// event channel on this surface, delivering EventApprovalRequested through
// the shared outbox exactly like REST's own deliverApprovalNotification —
// a human must actually see this, unlike every other event this surface
// has no live SSE subscriber to fan out to anyway.
func (s *Server) drainAndNotify(tenantID, sessionID uuid.UUID, chatID string, events <-chan RunEvent) {
	if s.Outbox == nil {
		for range events {
			// still drain fully: the channel must be closed by the run's own goroutine, and a receiver has to be here to let that happen
		}
		return
	}
	sender := &Sender{Channels: s.Channels, TenantID: tenantID, Client: s.HTTPClient}
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
		if err := s.Outbox.Deliver(context.Background(), tenantID, sessionID, re.Event.Seq, "telegram", chatID, payload, sender); err != nil {
			slog.Error("telegram: deliver approval notification", "error", err, "session_id", sessionID)
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
