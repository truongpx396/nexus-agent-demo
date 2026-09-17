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
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

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

	// Resume continues an existing conversational session found awaiting
	// the next message (handleWebhook's own session-key lookup) — nil
	// leaves that path unavailable, degrading every message to Starter's
	// always-fresh-session behavior (this field's own zero value, which
	// every pre-continuity caller has).
	Resume Resumer

	// Outbox, if set, backs durable at-least-once delivery of
	// EventApprovalRequested (README task 7.14, reused unchanged) — nil
	// leaves it unmounted. Unlike REST's own OutboxSender (a single
	// stateless field: its dev-mode logSender needs no per-tenant
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
	// sessionKey is deterministic per (tenant, chat) — every message from
	// this chat looks up the SAME key (store.GetSessionByKey), so a reply
	// resumes the open conversation instead of always starting a new one.
	// Tenant scoping comes from the lookup's own WHERE tenant_id=$1, not
	// this string, so no tenant id needs to be folded in here.
	sessionKey := "telegram:" + chatID

	if _, err := s.dispatch(r.Context(), tenantID, userID, sessionKey, chatID, upd.Message.Text); err != nil {
		log.Error().Err(err).Any("tenant_id", tenantID).Msg("telegram: dispatch")
		http.Error(w, "dispatch: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// dispatch looks up sessionKey's most recent session (store.GetSessionByKey)
// and resumes it via Resume.ResumeConversation when it's genuinely
// awaiting_input; every other case (no session yet, or one found but
// running/suspended/already terminal) falls through to startRun — a fresh
// conversational session reusing the same key, so the NEXT message finds
// it. s.Resume == nil (no pre-continuity caller sets it) always takes the
// fresh-session path, unchanged from before this feature existed.
func (s *Server) dispatch(ctx context.Context, tenantID, userID uuid.UUID, sessionKey, chatID, input string) (uuid.UUID, error) {
	if s.Resume != nil {
		var sess store.Session
		var found bool
		err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var derr error
			sess, found, derr = store.GetSessionByKey(ctx, tx, tenantID, sessionKey)
			return derr
		})
		if err != nil {
			return uuid.Nil, fmt.Errorf("look up session for %s: %w", sessionKey, err)
		}
		if found && sess.Status == store.SessionStatusAwaitingInput {
			return s.resumeRun(ctx, tenantID, sess.SessionID, chatID, input)
		}
	}
	return s.startRun(ctx, tenantID, userID, sessionKey, chatID, input)
}

// startRun mirrors internal/surfaces/rest's handleCreateRun (session + DEK
// creation, then RunStarter.StartRun) — duplicated per this codebase's
// established cross-surface idiom rather than imported, since REST and
// Telegram share no direct dependency. chatID is the Telegram delivery
// target for any outbox notification this run produces — carried alongside
// (never derived from) sessionID, since the two identify different things
// (surfaces.Outbox.Deliver's own recipient parameter). sessionKey is
// dispatch's own deterministic per-chat key (kernel.RunConfig.
// Conversational's pause-not-terminate semantics mean the NEXT message
// from this chat can find this session again via that key while it's
// awaiting_input).
func (s *Server) startRun(ctx context.Context, tenantID, userID uuid.UUID, sessionKey, chatID, input string) (uuid.UUID, error) {
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
			SessionID:      sessionID,
			SessionKey:     sessionKey,
			TenantID:       tenantID,
			SurfaceID:      "telegram",
			UserID:         userID,
			AgentVersion:   1,
			HarnessDigest:  digest,
			DataLabel:      string(provider.DataLabelInternal),
			RouteModelID:   route.ModelID,
			RouteReason:    route.Reason,
			AutonomyLevel:  "supervised",
			Conversational: true,
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

// resumeRun continues sessionID (already confirmed awaiting_input by
// dispatch) with the next chat message — Resume.ResumeConversation is
// cmd/nexusd's adapter over internal/runctl.Control.ResumeConversation,
// which rehydrates the session from its durable log itself; nothing here
// needs to reconstruct RunConfig the way startRun's own DEK-minting/
// harness-digest dance does.
func (s *Server) resumeRun(ctx context.Context, tenantID, sessionID uuid.UUID, chatID, input string) (uuid.UUID, error) {
	events, err := s.Resume.ResumeConversation(context.Background(), tenantID, sessionID, input) // a run outlives this webhook request
	if err != nil {
		return uuid.Nil, fmt.Errorf("resume conversation: %w", err)
	}
	go s.drainAndNotify(tenantID, sessionID, chatID, events)
	return sessionID, nil
}

// drainAndNotify is publishUntilDone's Telegram-side counterpart
// (internal/surfaces/rest/server.go): the only consumer of a run's event
// channel on this surface (fresh via startRun or resumed via resumeRun —
// identical either way), delivering EventApprovalRequested AND
// EventContent through the shared outbox — a human must actually see
// both, unlike every other event this surface has no live SSE subscriber
// to fan out to anyway.
func (s *Server) drainAndNotify(tenantID, sessionID uuid.UUID, chatID string, events <-chan RunEvent) {
	if s.Outbox == nil {
		for range events {
			// still drain fully: the channel must be closed by the run's own goroutine, and a receiver has to be here to let that happen
		}
		return
	}
	sender := &Sender{Channels: s.Channels, TenantID: tenantID, Client: s.HTTPClient}
	for re := range events {
		if re.Err != nil {
			continue
		}
		payload, ok := s.notificationPayload(context.Background(), tenantID, sessionID, re.Event)
		if !ok {
			continue
		}
		if err := s.Outbox.Deliver(context.Background(), tenantID, sessionID, re.Event.Seq, "telegram", chatID, payload, sender); err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("telegram: deliver notification")
		}
	}
}

// notificationPayload builds the payload for ONE event worth notifying the
// user about — ok=false means this event isn't one of those (every type
// other than approval_requested/content). EventContent's Body is sealed
// (kernel/events.go's contentPayload, appendEvent's own doc comment: the
// channel this surface reads carries the durable, ciphertext Event, never
// plaintext) — decrypted here the same way
// internal/surfaces/rest/run_events.go's toEventDTO already does, the only
// other place in this codebase that decrypts a store.Event outside the
// kernel itself. A decrypt failure is logged and the event skipped, never
// crashing the drain loop — this surface has no other way to surface that
// failure to anyone.
func (s *Server) notificationPayload(ctx context.Context, tenantID, sessionID uuid.UUID, ev store.Event) (payload []byte, ok bool) {
	switch ev.Type { //nolint:exhaustive // only these two event types are ever worth notifying a chat user about
	case store.EventApprovalRequested:
		toolID := ""
		if ev.ToolID != nil {
			toolID = *ev.ToolID
		}
		b, err := json.Marshal(notificationPayload{Kind: "approval", SessionID: sessionID.String(), ToolID: toolID})
		if err != nil {
			return nil, false
		}
		return b, true

	case store.EventContent:
		var dek crypto.DEK
		err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var derr error
			dek, derr = s.KeyStore.Unwrap(ctx, tx, ev.KeyID)
			return derr
		})
		if err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("telegram: unwrap key for content event")
			return nil, false
		}
		plaintext, err := crypto.Open(dek, ev.Payload, tenantID.String(), sessionID.String())
		if err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("telegram: decrypt content event")
			return nil, false
		}
		var body struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(plaintext, &body); err != nil {
			return nil, false
		}
		b, err := json.Marshal(notificationPayload{Kind: "content", Text: body.Body})
		if err != nil {
			return nil, false
		}
		return b, true

	default:
		return nil, false
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
