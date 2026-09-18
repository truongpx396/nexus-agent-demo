// Package zalo is a webhook surface (README Phase 11, task 11.4),
// structurally the same shape as internal/surfaces/telegram (its own doc
// comment) with one thing genuinely different: how inbound authenticity is
// verified. Zalo's Official Account platform signs each webhook body with
// an HMAC over the app secret rather than a static header token; this
// package implements the general shared-secret HMAC-over-raw-body pattern
// that models — see verifySignature's own doc comment for the honesty note
// on what is and isn't claimed about byte-exact fidelity to Zalo's own
// wire format. Zero kernel change — this package never imports kernel/.
package zalo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// zaloNamespace is telegram.telegramNamespace's own counterpart — a fixed
// UUID namespace for deriving a stable per-(tenant-OA,sender.id) UserID,
// recomputed fresh from every inbound event rather than stored in a
// mapping row (task 7.13).
var zaloNamespace = uuid.MustParse("6f8e1a2b-0000-4000-8000-000000000002")

// inboundEvent is the minimal subset of a Zalo OA "user_send_text" webhook
// event this surface reads.
type inboundEvent struct {
	AppID     string `json:"app_id"`
	EventName string `json:"event_name"`
	Sender    struct {
		ID string `json:"id"`
	} `json:"sender"`
	Message struct {
		Text  string `json:"text"`
		MsgID string `json:"msg_id"`
	} `json:"message"`
}

// ChannelPort resolves the tenant's admitted Zalo credentials —
// structurally identical to internal/surfaces/telegram.ChannelPort, into
// the same migrations/0021_messaging_channels.sql table (kind='zalo').
type ChannelPort interface {
	// AppSecret returns the tenant's configured OA app secret, unsealed —
	// used both to verify an inbound signature and (via Sender) to
	// authenticate outbound sends. ok=false means no active zalo channel
	// is configured for tenantID at all.
	AppSecret(ctx context.Context, tenantID uuid.UUID) (secret string, ok bool, err error)
	// AccessToken returns the tenant's current OA send-API access token,
	// unsealed — used only by Sender.Send.
	AccessToken(ctx context.Context, tenantID uuid.UUID) (token string, err error)
}

// Server holds everything one Zalo webhook handler needs.
type Server struct {
	Store                 *store.Store
	KeyStore              *crypto.KeyStore
	Starter               RunStarter
	Channels              ChannelPort
	CatalogManifestDigest []byte

	// Outbox, if set, backs durable at-least-once delivery of
	// EventApprovalRequested — see internal/surfaces/telegram.Server's own
	// doc comment on why a *Sender is constructed per-delivery here rather
	// than held as one fixed field.
	Outbox     *surfaces.Outbox
	HTTPClient *http.Client

	RateLimit *RateLimiter

	// Resume continues an existing conversational session found awaiting
	// the next message — see internal/surfaces/telegram.Server's own doc
	// comment; nil (every pre-continuity caller) always takes the
	// always-fresh-session path.
	Resume Resumer

	// Lock, if set, serializes dispatch's own decide-then-act sequence and
	// the turn it kicks off around sessionKey (surfaces.AcquireSessionLock)
	// — cmd/nexusd wires the same *queue.SessionLock instance the
	// crash-recovery queue worker already holds turns through
	// (internal/queue/worker.go), closing the production-readiness
	// review's finding that "the REST/webhook direct-call path bypasses
	// [SessionLock] entirely." nil (every pre-this-fix caller and test)
	// reproduces the prior unlocked behavior exactly.
	Lock surfaces.Locker
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/webhooks/zalo/{tenant_id}", s.handleWebhook)
	return mux
}

// verifySignature checks an HMAC-SHA256-over-raw-body signature against
// the tenant's app secret — the general shared-secret webhook-authenticity
// pattern this surface models (docs/constitution.md's "verify provider
// authenticity ... before the kernel sees the payload"). Honesty note,
// matching this codebase's own documented-gap convention: Zalo OA's real
// wire format computes its "mac" field over a specific ordered subset of
// the JSON body's own fields, not the raw byte stream — reproducing that
// exact algorithm needs Zalo's own current API reference in hand, which
// this implementation does not claim byte-exact fidelity to. What IS real
// here is the security property that matters for this phase: the header
// is verified against a per-tenant secret, in constant time, before the
// body is parsed — the same shape a byte-exact implementation would need
// regardless.
func verifySignature(body []byte, secret, headerSig string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(headerSig))
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.PathValue("tenant_id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}

	secret, ok, err := s.Channels.AppSecret(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "resolve channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no zalo channel configured for this tenant", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	// Signature is verified over the raw body BEFORE any JSON parsing is
	// attempted — same ordering discipline as Telegram's header check.
	if !verifySignature(body, secret, r.Header.Get("X-ZEvent-Signature")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.RateLimit != nil && !s.RateLimit.Allow(tenantID.String()) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	var ev inboundEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	if ev.Message.Text == "" {
		w.WriteHeader(http.StatusOK) // not every OA event is a text message worth starting a run over
		return
	}

	userID := uuid.NewSHA1(zaloNamespace, fmt.Appendf(nil, "%s:%s", tenantID, ev.Sender.ID))
	// sessionKey is deterministic per (tenant, sender) — see
	// internal/surfaces/telegram/webhook.go's own doc comment on this
	// field for the full rationale.
	sessionKey := "zalo:" + ev.Sender.ID
	// deliveryID is Zalo's own msg_id — the provider-native id ClaimDelivery
	// dedupes a redelivered event against. Existing test fixtures don't set
	// this field, so it defaults to "" and store.ClaimInboundDelivery's own
	// documented empty-string behavior (always claims, never dedupes)
	// applies — intentional, not a gap this change introduces.
	deliveryID := ev.Message.MsgID

	if _, err := s.dispatch(r.Context(), tenantID, userID, sessionKey, deliveryID, ev.Sender.ID, ev.Message.Text); err != nil {
		log.Error().Err(err).Any("tenant_id", tenantID).Msg("zalo: dispatch")
		http.Error(w, "dispatch: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// dispatch first claims deliveryID (surfaces.ClaimDelivery) — a provider
// redelivery of an event this surface already accepted is acknowledged
// (uuid.Nil, nil) without ever reaching the session lookup below, closing
// the production-readiness review's finding that "Telegram/Zalo webhooks
// retry by design" and nothing dedupes that. A freshly-claimed delivery
// then acquires s.Lock (if set) around the ENTIRE decide-then-act sequence
// that follows — including the turn dispatch kicks off, which runs to
// completion in its own goroutine well after this function returns
// (startRun/resumeRun's own doc comments), which is why release is a
// closure threaded through to drainAndNotify rather than a plain defer
// here. Lock contention (ok=false) means another goroutine is ALREADY
// driving this exact session_key's turn; this delivery is logged and
// dropped rather than risk a second concurrent turn for it — a rare,
// bounded, logged trade-off, not a silent one.
//
// Otherwise mirrors internal/surfaces/telegram's own (its doc comment) —
// duplicated per this codebase's established cross-surface idiom.
func (s *Server) dispatch(ctx context.Context, tenantID, userID uuid.UUID, sessionKey, deliveryID, recipientID, input string) (uuid.UUID, error) {
	claimed, err := surfaces.ClaimDelivery(ctx, s.Store, tenantID, Descriptor.SurfaceID, deliveryID, sessionKey)
	if err != nil {
		return uuid.Nil, fmt.Errorf("claim delivery: %w", err)
	}
	if !claimed {
		log.Info().Any("tenant_id", tenantID).Str("delivery_id", deliveryID).Msg("zalo: duplicate delivery acknowledged without dispatching")
		return uuid.Nil, nil
	}

	release := func() {}
	if s.Lock != nil {
		token, ok, lerr := surfaces.AcquireSessionLock(ctx, s.Lock, sessionKey)
		if lerr != nil {
			return uuid.Nil, fmt.Errorf("acquire session lock: %w", lerr)
		}
		if !ok {
			log.Warn().Any("tenant_id", tenantID).Str("session_key", sessionKey).Msg("zalo: session lock contended; dropping this delivery rather than risk a concurrent turn")
			return uuid.Nil, nil
		}
		release = func() {
			if rerr := s.Lock.Release(context.Background(), sessionKey, token); rerr != nil {
				log.Error().Err(rerr).Str("session_key", sessionKey).Msg("zalo: session lock release failed")
			}
		}
	}

	if s.Resume != nil {
		var sess store.Session
		var found bool
		err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var derr error
			sess, found, derr = store.GetSessionByKey(ctx, tx, tenantID, sessionKey)
			return derr
		})
		if err != nil {
			release()
			return uuid.Nil, fmt.Errorf("look up session for %s: %w", sessionKey, err)
		}
		if found && sess.Status == store.SessionStatusAwaitingInput {
			return s.resumeRun(ctx, tenantID, sess.SessionID, recipientID, input, release)
		}
	}
	return s.startRun(ctx, tenantID, userID, sessionKey, recipientID, input, release)
}

// startRun mirrors internal/surfaces/telegram's own (its doc comment) —
// duplicated per this codebase's established cross-surface idiom.
// recipientID is the Zalo user id this run's own outbox delivery (if any)
// sends back to.
func (s *Server) startRun(ctx context.Context, tenantID, userID uuid.UUID, sessionKey, recipientID, input string, release func()) (uuid.UUID, error) {
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
			SurfaceID:      "zalo",
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
		release()
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
		release()
		return uuid.Nil, fmt.Errorf("start run: %w", err)
	}
	go s.drainAndNotify(tenantID, sessionID, recipientID, events, release)
	return sessionID, nil
}

// resumeRun mirrors internal/surfaces/telegram's own (its doc comment).
func (s *Server) resumeRun(ctx context.Context, tenantID, sessionID uuid.UUID, recipientID, input string, release func()) (uuid.UUID, error) {
	events, err := s.Resume.ResumeConversation(context.Background(), tenantID, sessionID, input)
	if err != nil {
		release()
		return uuid.Nil, fmt.Errorf("resume conversation: %w", err)
	}
	go s.drainAndNotify(tenantID, sessionID, recipientID, events, release)
	return sessionID, nil
}

// drainAndNotify mirrors internal/surfaces/telegram's own (its doc
// comment) — the only consumer of a run's event channel on this surface,
// fresh or resumed alike.
func (s *Server) drainAndNotify(tenantID, sessionID uuid.UUID, recipientID string, events <-chan RunEvent, release func()) {
	defer release()
	if s.Outbox == nil {
		for range events {
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
		if err := s.Outbox.Deliver(context.Background(), tenantID, sessionID, re.Event.Seq, "zalo", recipientID, payload, sender); err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("zalo: deliver notification")
		}
	}
}

// notificationPayload mirrors internal/surfaces/telegram's own (its doc
// comment) — decrypts EventContent the same way
// internal/surfaces/rest/run_events.go's toEventDTO does.
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
			log.Error().Err(err).Any("session_id", sessionID).Msg("zalo: unwrap key for content event")
			return nil, false
		}
		plaintext, err := crypto.Open(dek, ev.Payload, tenantID.String(), sessionID.String())
		if err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("zalo: decrypt content event")
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
