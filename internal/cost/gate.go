package cost

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// DefaultCurrency is used wherever a caller doesn't specify one — this
// demo is single-currency throughout (FX is explicitly out of scope,
// README §8).
const DefaultCurrency = "USD"

// Purpose classifies WHY a Reserve call is happening — README task 4.8's
// "every model call metered: compaction, the safety model leg, prompt
// hooks, the judge, title generation" seam. PurposeTurn is the only value
// any call site in this codebase actually passes this phase
// (kernel/loop.go's turn loop, the one real Provider.Stream call site that
// exists today — tests/contract/cost_metering_test.go's AST check proves
// it). The rest exist so a later phase's real call site (compaction ships
// Phase 7; the safety model leg's demoSafetyModel stub in cmd/nexusd never
// calls a model; a prompt hook handler and the eval judge are Phase
// 3/9/10 territory) has a Purpose to pass without this type needing to
// grow again — "off the paying loop" must mean "a cheaper meter," never
// "no meter."
type Purpose string

const (
	PurposeTurn        Purpose = "turn"
	PurposeCompaction  Purpose = "compaction"
	PurposeSafetyModel Purpose = "safety_model"
	PurposeHookPrompt  Purpose = "hook_prompt"
	PurposeJudge       Purpose = "judge"
	PurposeTitle       Purpose = "title"

	// PurposeEmbedding is README task 12.4's real call site: unlike the
	// stub purposes above, internal/retrieval.Retriever.Search actually
	// calls Reserve with this purpose before every provider.Embedder.Embed
	// call — the embedding call site tests/contract's AST check (extended
	// from task 4.8's original) verifies is never reachable unmetered.
	PurposeEmbedding Purpose = "embedding"
)

// ReserveRequest is everything Reserve needs to estimate and check a
// worst-case pre-spend reservation for one upcoming Provider.Stream call.
type ReserveRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	ModelID   string
	Purpose   Purpose
}

// Reservation is Reserve's result and Reconcile's input. Both the
// successful and the refused case return a populated Reservation — a
// caller always has enough to append a store.EventBudgetDecision regardless
// of outcome (README task 4.6: "every gate resolution").
type Reservation struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	SessionID uuid.UUID
	ModelID   string

	Decision Decision

	sessionBudget *Budget // nil if no session-scoped budget existed
	tenantBudget  *Budget // nil if no tenant-scoped budget existed
	tenantEpoch   int64   // the epoch the tenant-level Redis reservation was taken under
}

// GateConfig is a Gate's construction-time tuning.
type GateConfig struct {
	Currency string // defaults to DefaultCurrency

	// MaxInputTokenEstimate/MaxOutputTokenEstimate size the worst-case
	// reservation Reserve prices BEFORE a call: the full estimated input is
	// priced as if none of it were cache-read (the worst case for cost),
	// plus a conservative output ceiling. A production deployment would
	// derive these from the routed model's real context window and a
	// configured max_output_tokens; neither is carried on
	// internal/provider's config yet, so this demo uses fixed, documented
	// defaults instead of inventing that plumbing as a Phase 4 side quest.
	MaxInputTokenEstimate  int64
	MaxOutputTokenEstimate int64

	// MaxEmbeddingTokenEstimate is the same worst-case-ceiling idea applied
	// to a PurposeEmbedding reservation (task 12.4): one fixed, documented
	// per-call ceiling regardless of how many texts a given Embed call
	// happens to batch — Reserve has never sized itself off the ACTUAL
	// prompt either (MaxInputTokenEstimate above is fixed for the same
	// reason), so embeddings inherit that same pre-call-not-post-call
	// philosophy rather than inventing a new one.
	MaxEmbeddingTokenEstimate int64

	// DegradeThresholdPercent: a reservation that pushes either ceiling's
	// spend to at least this percent (integer, e.g. 80) — without going
	// over — resolves DecisionDegrade instead of DecisionAllow. Integer
	// percent, never a float (this package's own ban, money_notfloat_test.go).
	DegradeThresholdPercent int64

	RedisTimeout time.Duration
}

func (c GateConfig) withDefaults() GateConfig {
	if c.Currency == "" {
		c.Currency = DefaultCurrency
	}
	if c.MaxInputTokenEstimate <= 0 {
		c.MaxInputTokenEstimate = 8_000
	}
	if c.MaxOutputTokenEstimate <= 0 {
		c.MaxOutputTokenEstimate = 4_096
	}
	if c.MaxEmbeddingTokenEstimate <= 0 {
		c.MaxEmbeddingTokenEstimate = 8_000
	}
	if c.DegradeThresholdPercent <= 0 {
		c.DegradeThresholdPercent = 80
	}
	return c
}

// Gate is the reserve-then-reconcile engine (README task 4.4) — the real
// BudgetGate kernel/types.go seams for. Construct one per process and
// share it across every session; it owns three caches (per-tenant
// PriceBook, per-session budget+running-spend, per-tenant budget) filled
// lazily on first touch and never invalidated within a process lifetime —
// matching internal/tools/pipeline.go's own per-session state, which is
// process-lifetime, not hot-reloaded.
//
// Gate owns its own Postgres transactions via Store rather than taking a
// caller-supplied tx the way internal/crypto.KeyStore's methods do: the
// abstract BudgetGate.Reserve/Reconcile signature (README §4) takes no tx
// parameter, because one Reserve call is its own independent unit of
// work — not a step nested inside a broader caller transaction the way
// "seal this event's payload" is. budget_decisions and cost_records
// therefore commit in Gate's own transactions, separate from the kernel's
// EventBudgetDecision append; internal/cost never writes to the events
// table itself (store.Append is the log's one sanctioned writer,
// docs/constitution.md Principle II). Both commit durably as part of
// handling one Reserve/Reconcile call, so a crash between them can only
// ever leave a decision/record row with no matching kernel event (the cost
// was still accounted for) — never the reverse.
type Gate struct {
	store  *store.Store
	redis  scripter
	meters *Registry
	cfg    GateConfig

	mu           sync.Mutex
	priceBooks   map[uuid.UUID]*PriceBook // by tenant
	sessionState map[uuid.UUID]sessionCeilState
	tenantCache  map[uuid.UUID]*Budget // by tenant; a cached nil means "looked up, none exists"
	armed        map[uuid.UUID]bool    // budget IDs already Arm()-ed this process
}

type sessionCeilState struct {
	budget *Budget
	spent  Money
}

func NewGate(st *store.Store, redisClient *redis.Client, meters *Registry, cfg GateConfig) *Gate {
	if meters == nil {
		meters = DefaultMeters()
	}
	return &Gate{
		store:        st,
		redis:        newRedisScripter(redisClient, cfg.RedisTimeout),
		meters:       meters,
		cfg:          cfg.withDefaults(),
		priceBooks:   map[uuid.UUID]*PriceBook{},
		sessionState: map[uuid.UUID]sessionCeilState{},
		tenantCache:  map[uuid.UUID]*Budget{},
		armed:        map[uuid.UUID]bool{},
	}
}
