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
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

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
	// sessionKey is deterministic per (tenant, sender address) — see
	// internal/surfaces/telegram/webhook.go's own doc comment on this
	// field for the full rationale. Lower-cased so the same address always
	// maps to the same key regardless of casing.
	sessionKey := "email:" + strings.ToLower(msg.From)
	// deliveryID is the inbound provider's own Message-ID — the
	// provider-native id ClaimDelivery dedupes a redelivered message
	// against. May legitimately be empty for some inbound-parse providers;
	// store.ClaimInboundDelivery's own documented empty-string behavior
	// (always claims, never dedupes) applies in that case.
	deliveryID := msg.MessageID

	if _, err := s.dispatch(r.Context(), tenantID, userID, sessionKey, deliveryID, msg.From, input); err != nil {
		log.Error().Err(err).Any("tenant_id", tenantID).Msg("email: dispatch")
		http.Error(w, "dispatch: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// dispatch first claims deliveryID (surfaces.ClaimDelivery) — a provider
// redelivery of a message this surface already accepted is acknowledged
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
func (s *Server) dispatch(ctx context.Context, tenantID, userID uuid.UUID, sessionKey, deliveryID, recipientAddress, input string) (uuid.UUID, error) {
	claimed, err := surfaces.ClaimDelivery(ctx, s.Store, tenantID, Descriptor.SurfaceID, deliveryID, sessionKey)
	if err != nil {
		return uuid.Nil, fmt.Errorf("claim delivery: %w", err)
	}
	if !claimed {
		log.Info().Any("tenant_id", tenantID).Str("delivery_id", deliveryID).Msg("email: duplicate delivery acknowledged without dispatching")
		return uuid.Nil, nil
	}

	release := func() {}
	if s.Lock != nil {
		token, ok, lerr := surfaces.AcquireSessionLock(ctx, s.Lock, sessionKey)
		if lerr != nil {
			return uuid.Nil, fmt.Errorf("acquire session lock: %w", lerr)
		}
		if !ok {
			log.Warn().Any("tenant_id", tenantID).Str("session_key", sessionKey).Msg("email: session lock contended; dropping this delivery rather than risk a concurrent turn")
			return uuid.Nil, nil
		}
		release = func() {
			if rerr := s.Lock.Release(context.Background(), sessionKey, token); rerr != nil {
				log.Error().Err(rerr).Str("session_key", sessionKey).Msg("email: session lock release failed")
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
			return s.resumeRun(ctx, tenantID, sess.SessionID, recipientAddress, input, release)
		}
	}
	return s.startRun(ctx, tenantID, userID, sessionKey, recipientAddress, input, release)
}

// startRun mirrors every other surface's own (their doc comments) —
// duplicated per this codebase's established cross-surface idiom.
// recipientAddress is the inbound sender's own address, used as this run's
// outbox delivery recipient for any reply.
func (s *Server) startRun(ctx context.Context, tenantID, userID uuid.UUID, sessionKey, recipientAddress, input string, release func()) (uuid.UUID, error) {
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
			SurfaceID:      "email",
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
	go s.drainAndNotify(tenantID, sessionID, recipientAddress, events, release)
	return sessionID, nil
}

// resumeRun mirrors internal/surfaces/telegram's own (its doc comment).
func (s *Server) resumeRun(ctx context.Context, tenantID, sessionID uuid.UUID, recipientAddress, input string, release func()) (uuid.UUID, error) {
	events, err := s.Resume.ResumeConversation(context.Background(), tenantID, sessionID, input)
	if err != nil {
		release()
		return uuid.Nil, fmt.Errorf("resume conversation: %w", err)
	}
	go s.drainAndNotify(tenantID, sessionID, recipientAddress, events, release)
	return sessionID, nil
}

// drainAndNotify mirrors internal/surfaces/telegram's own (its doc
// comment) — the only consumer of a run's event channel on this surface,
// fresh or resumed alike.
func (s *Server) drainAndNotify(tenantID, sessionID uuid.UUID, recipientAddress string, events <-chan RunEvent, release func()) {
	defer release()
	if s.Outbox == nil {
		for range events {
		}
		return
	}
	sender := &Sender{Channels: s.Channels, TenantID: tenantID}
	for re := range events {
		if re.Err != nil {
			continue
		}
		payload, ok := s.notificationPayload(context.Background(), tenantID, sessionID, re.Event)
		if !ok {
			continue
		}
		if err := s.Outbox.Deliver(context.Background(), tenantID, sessionID, re.Event.Seq, "email", recipientAddress, payload, sender); err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("email: deliver notification")
		}
	}
}

// notificationPayload mirrors internal/surfaces/telegram's own (its doc
// comment) — decrypts EventContent the same way
// internal/surfaces/rest/run_events.go's toEventDTO does.
func (s *Server) notificationPayload(ctx context.Context, tenantID, sessionID uuid.UUID, ev store.Event) (payload []byte, ok bool) {
	switch ev.Type { //nolint:exhaustive // only these two event types are ever worth notifying a user about
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
			log.Error().Err(err).Any("session_id", sessionID).Msg("email: unwrap key for content event")
			return nil, false
		}
		plaintext, err := crypto.Open(dek, ev.Payload, tenantID.String(), sessionID.String())
		if err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("email: decrypt content event")
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
