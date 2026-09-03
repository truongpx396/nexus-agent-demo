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
		Text string `json:"text"`
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

	if _, err := s.startRun(r.Context(), tenantID, userID, ev.Sender.ID, ev.Message.Text); err != nil {
		slog.Error("zalo: start run", "error", err, "tenant_id", tenantID)
		http.Error(w, "start run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// startRun mirrors internal/surfaces/telegram's own (its doc comment) —
// duplicated per this codebase's established cross-surface idiom.
// recipientID is the Zalo user id this run's own outbox delivery (if any)
// sends back to.
func (s *Server) startRun(ctx context.Context, tenantID, userID uuid.UUID, recipientID, input string) (uuid.UUID, error) {
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
			SurfaceID:     "zalo",
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
	go s.drainAndNotify(tenantID, sessionID, recipientID, events)
	return sessionID, nil
}

func (s *Server) drainAndNotify(tenantID, sessionID uuid.UUID, recipientID string, events <-chan RunEvent) {
	if s.Outbox == nil {
		for range events {
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
		if err := s.Outbox.Deliver(context.Background(), tenantID, sessionID, re.Event.Seq, "zalo", recipientID, payload, sender); err != nil {
			slog.Error("zalo: deliver approval notification", "error", err, "session_id", sessionID)
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
