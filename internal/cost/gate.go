package cost

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
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

// Reserve estimates a worst-case cost for one upcoming model call and
// checks it against whatever budgets apply (session-scoped: local,
// synchronous, task 4.5; tenant-scoped: Redis-atomic, task 4.4), in that
// order. It ALWAYS returns a populated Reservation; the returned error is
// non-nil precisely when Decision.Kind == DecisionRefuseCeiling.
func (g *Gate) Reserve(ctx context.Context, req ReserveRequest) (Reservation, error) {
	res := Reservation{ID: uuid.New(), TenantID: req.TenantID, SessionID: req.SessionID, ModelID: req.ModelID}
	zero := Money{Currency: g.cfg.Currency}

	pb, err := g.priceBookFor(ctx, req.TenantID)
	if err != nil {
		return g.refuse(ctx, res, nil, zero, "price book unavailable (fail closed): "+err.Error())
	}
	worst, err := g.worstCaseCost(pb, req.ModelID, req.Purpose)
	if err != nil {
		return g.refuse(ctx, res, nil, zero, "cannot price a worst-case reservation (fail closed): "+err.Error())
	}

	sessBudget, err := g.sessionBudgetFor(ctx, req.TenantID, req.SessionID)
	if err != nil {
		return g.refuse(ctx, res, nil, worst, "session budget lookup failed (fail closed): "+err.Error())
	}
	tenBudget, err := g.tenantBudgetFor(ctx, req.TenantID)
	if err != nil {
		return g.refuse(ctx, res, nil, worst, "tenant budget lookup failed (fail closed): "+err.Error())
	}

	if sessBudget == nil && tenBudget == nil {
		res.Decision = Decision{Kind: DecisionSkip, Reason: "no session- or tenant-scoped budget configured", Reserved: zero}
		g.bestEffortPersist(ctx, req.TenantID, req.SessionID, res.Decision)
		return res, nil
	}

	// --- session-level: local, synchronous, no I/O beyond the one-time
	// lazy load above (task 4.5 — "a ceiling never depends on a round trip").
	var tentativeSessionSpent Money
	softHit := false
	if sessBudget != nil {
		g.mu.Lock()
		current := g.sessionState[req.SessionID].spent
		g.mu.Unlock()
		if current.Currency == "" {
			current = Money{Currency: sessBudget.Ceiling.Currency}
		}
		tentativeSessionSpent, _ = current.Add(worst)
		if tentativeSessionSpent.Micros > sessBudget.Ceiling.Micros {
			return g.refuse(ctx, res, &sessBudget.ID, worst, fmt.Sprintf(
				"session budget ceiling %s exceeded (this reservation would reach %s)", sessBudget.Ceiling, tentativeSessionSpent))
		}
		if tentativeSessionSpent.Micros*100 >= sessBudget.Ceiling.Micros*g.cfg.DegradeThresholdPercent {
			softHit = true
		}
	}

	// --- tenant-level: Redis, atomic (task 4.4).
	reservedInRedis := false
	if tenBudget != nil {
		if err := g.ensureArmed(ctx, req.TenantID, tenBudget); err != nil {
			return g.refuse(ctx, res, &tenBudget.ID, worst, "tenant budget epoch unavailable (fail closed): "+err.Error())
		}
		outcome, total, rerr := g.redis.Reserve(ctx, tenBudget.ID, tenBudget.Epoch, worst.Micros, tenBudget.Ceiling.Micros)
		if rerr != nil {
			return g.refuse(ctx, res, &tenBudget.ID, worst, "tenant budget check failed (fail closed): "+rerr.Error())
		}
		switch outcome {
		case reserveUnavailable:
			return g.refuse(ctx, res, &tenBudget.ID, worst, "tenant budget epoch unavailable (fail closed, never assumed zero spend)")
		case reserveStaleEpoch:
			return g.refuse(ctx, res, &tenBudget.ID, worst, "tenant budget epoch changed since last check (fail closed)")
		case reserveOverCeiling:
			return g.refuse(ctx, res, &tenBudget.ID, worst, fmt.Sprintf(
				"tenant budget ceiling %s exceeded (already at %s)", tenBudget.Ceiling, Money{Micros: total, Currency: tenBudget.Ceiling.Currency}))
		case reserveOK:
			reservedInRedis = true
			if total*100 >= tenBudget.Ceiling.Micros*g.cfg.DegradeThresholdPercent {
				softHit = true
			}
		}
		res.tenantEpoch = tenBudget.Epoch
	}

	deciding := sessBudget
	if deciding == nil {
		deciding = tenBudget
	}
	kind := DecisionAllow
	if softHit {
		kind = DecisionDegrade
	}
	decision := Decision{Kind: kind, Reason: decisionReason(kind, sessBudget, tenBudget), BudgetID: &deciding.ID, Reserved: worst}

	if err := g.persistDecision(ctx, req.TenantID, req.SessionID, decision); err != nil {
		// The reservation itself must not silently stand if we can't even
		// durably record that it happened — release what Redis already
		// holds and fail closed, same as any other accounting failure.
		if reservedInRedis {
			if rerr := g.redis.Release(ctx, tenBudget.ID, tenBudget.Epoch, worst.Micros); rerr != nil {
				slog.Error("cost: failed to roll back tenant reservation after a decision-persist failure", "error", rerr)
			}
		}
		return g.refuse(ctx, res, &deciding.ID, worst, "failed to record budget decision (fail closed): "+err.Error())
	}

	if sessBudget != nil {
		g.mu.Lock()
		g.sessionState[req.SessionID] = sessionCeilState{budget: sessBudget, spent: tentativeSessionSpent}
		g.mu.Unlock()
	}

	res.Decision = decision
	res.sessionBudget = sessBudget
	res.tenantBudget = tenBudget
	return res, nil
}

// Reconcile prices the real usage from a completed Provider.Stream call and
// releases the unused portion of what Reserve reserved. reported=false is
// the UNREPORTED case (task 4.7): the stream failed after the commit point
// with no trustworthy usage figures, so Reconcile charges the full
// reserved worst case instead — "an unreliable provider must not look
// free."
func (g *Gate) Reconcile(ctx context.Context, res Reservation, usage provider.Usage, reported bool) error {
	subject := res.ModelID
	if subject == "" {
		subject = WildcardSubject
	}

	var records []CostRecord
	actual := Money{Currency: g.cfg.Currency}

	if !reported {
		actual = res.Decision.Reserved
		if actual.Currency == "" {
			actual.Currency = g.cfg.Currency
		}
		records = []CostRecord{{
			Meter: MeterUnreportedReservation, Quantity: 1, Unit: "reservation",
			ModelID: res.ModelID, Unreported: true, ReservationID: &res.ID, Cost: actual,
		}}
	} else {
		pb, err := g.priceBookFor(ctx, res.TenantID)
		if err != nil {
			return fmt.Errorf("cost: reconcile: price book unavailable: %w", err)
		}
		now := time.Now()
		for _, u := range []struct {
			meter MeterID
			qty   int64
		}{
			{MeterInputUncached, int64(usage.InputUncached)},
			{MeterInputCacheRead, int64(usage.InputCacheRead)},
			{MeterInputCacheWrite, int64(usage.InputCacheWrite)},
			{MeterOutput, int64(usage.OutputTokens)},
		} {
			if u.qty == 0 {
				continue
			}
			m, ok := g.meters.Lookup(u.meter)
			if !ok {
				return fmt.Errorf("cost: reconcile: %w", errUnknownMeter(u.meter))
			}
			c, cerr := pb.Cost(u.meter, subject, u.qty, now)
			if cerr != nil {
				return fmt.Errorf("cost: reconcile: %w", cerr)
			}
			records = append(records, CostRecord{
				Meter: u.meter, Quantity: u.qty, Unit: m.Unit, ModelID: res.ModelID, ReservationID: &res.ID, Cost: c,
			})
			actual, _ = actual.Add(c)
		}
	}

	if len(records) > 0 {
		if err := g.store.InTenantTx(ctx, res.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return RecordUsage(ctx, tx, res.TenantID, res.SessionID, records)
		}); err != nil {
			return fmt.Errorf("cost: reconcile: %w", err)
		}
	}

	return g.finishReconcile(ctx, res, actual)
}

// ReconcileEmbedding is Reconcile's counterpart for a PurposeEmbedding
// reservation (task 12.4): a single meter (MeterEmbeddingTokens) priced off
// the real token count internal/provider.EmbedUsage reports, rather than
// Reconcile's four-way chat split — an embedding call has nothing to split.
// reported=false is the same UNREPORTED case Reconcile's own doc comment
// describes: charge the full reserved worst case rather than assume a
// failed call was free.
func (g *Gate) ReconcileEmbedding(ctx context.Context, res Reservation, tokensUsed int, reported bool) error {
	subject := res.ModelID
	if subject == "" {
		subject = WildcardSubject
	}

	var records []CostRecord
	actual := Money{Currency: g.cfg.Currency}

	if !reported {
		actual = res.Decision.Reserved
		if actual.Currency == "" {
			actual.Currency = g.cfg.Currency
		}
		records = []CostRecord{{
			Meter: MeterUnreportedReservation, Quantity: 1, Unit: "reservation",
			ModelID: res.ModelID, Unreported: true, ReservationID: &res.ID, Cost: actual,
		}}
	} else if tokensUsed > 0 {
		pb, err := g.priceBookFor(ctx, res.TenantID)
		if err != nil {
			return fmt.Errorf("cost: reconcile embedding: price book unavailable: %w", err)
		}
		c, err := pb.Cost(MeterEmbeddingTokens, subject, int64(tokensUsed), time.Now())
		if err != nil {
			return fmt.Errorf("cost: reconcile embedding: %w", err)
		}
		records = []CostRecord{{
			Meter: MeterEmbeddingTokens, Quantity: int64(tokensUsed), Unit: "tokens",
			ModelID: res.ModelID, ReservationID: &res.ID, Cost: c,
		}}
		actual = c
	}

	if len(records) > 0 {
		if err := g.store.InTenantTx(ctx, res.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return RecordUsage(ctx, tx, res.TenantID, res.SessionID, records)
		}); err != nil {
			return fmt.Errorf("cost: reconcile embedding: %w", err)
		}
	}

	return g.finishReconcile(ctx, res, actual)
}

// finishReconcile is Reconcile and ReconcileEmbedding's shared tail: release
// the unused portion of what Reserve reserved (worst case minus actual)
// against whichever budget(s) backed the reservation. Both callers have
// already durably recorded their own CostRecords before reaching here —
// this only ever touches the reservation counters, never cost_records
// again.
func (g *Gate) finishReconcile(ctx context.Context, res Reservation, actual Money) error {
	if res.Decision.Kind == DecisionSkip || res.Decision.Kind == DecisionRefuseCeiling {
		return nil // nothing was reserved against a counter to release
	}

	reserved := res.Decision.Reserved
	if reserved.Currency == "" {
		reserved.Currency = g.cfg.Currency
	}
	delta, err := reserved.Sub(actual)
	if err != nil {
		return fmt.Errorf("cost: reconcile: %w", err)
	}

	if res.sessionBudget != nil {
		g.mu.Lock()
		st := g.sessionState[res.SessionID]
		if adjusted, aerr := st.spent.Sub(delta); aerr == nil {
			st.spent = adjusted
			g.sessionState[res.SessionID] = st
		}
		g.mu.Unlock()
	}
	if res.tenantBudget != nil {
		if err := g.redis.Release(ctx, res.tenantBudget.ID, res.tenantEpoch, delta.Micros); err != nil {
			slog.Error("cost: failed to release tenant reservation delta", "error", err, "tenant_id", res.TenantID, "session_id", res.SessionID)
		}
	}
	return nil
}

// Record durably logs qty units of a meter with no pre-spend reservation —
// the abstract BudgetGate.Record method (README §4), used for a
// non-reservable meter (MeterSandboxSeconds, MeterToolInvocations) whose
// true cost is only knowable after the fact. Nothing in this phase calls
// it (task 4.2's "registered but unemitted"); it exists so the seam is
// real and independently testable rather than merely declared.
func (g *Gate) Record(ctx context.Context, tenantID, sessionID uuid.UUID, meter MeterID, qty int64, modelID string) error {
	m, ok := g.meters.Lookup(meter)
	if !ok {
		return errUnknownMeter(meter)
	}
	pb, err := g.priceBookFor(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("cost: record: price book unavailable: %w", err)
	}
	subject := modelID
	if subject == "" {
		subject = WildcardSubject
	}
	c, err := pb.Cost(meter, subject, qty, time.Now())
	if err != nil {
		return fmt.Errorf("cost: record: %w", err)
	}
	return g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordUsage(ctx, tx, tenantID, sessionID, []CostRecord{{Meter: meter, Quantity: qty, Unit: m.Unit, ModelID: modelID, Cost: c}})
	})
}

// --- internal helpers ---

func (g *Gate) worstCaseCost(pb *PriceBook, modelID string, purpose Purpose) (Money, error) {
	subject := modelID
	if subject == "" {
		subject = WildcardSubject
	}
	now := time.Now()

	// An embedding call has no output half and no cache to price around —
	// task 12.4's worst case is just its one reservable meter, sized off
	// MaxEmbeddingTokenEstimate exactly the way the chat path below is
	// sized off MaxInputTokenEstimate/MaxOutputTokenEstimate.
	if purpose == PurposeEmbedding {
		return pb.Cost(MeterEmbeddingTokens, subject, g.cfg.MaxEmbeddingTokenEstimate, now)
	}

	// Worst case assumes NO cache benefit — the whole estimated input
	// priced as MeterInputUncached, never InputCacheRead — because a
	// reservation exists to bound the call BEFORE the provider tells us
	// whether the cache actually hit.
	in, err := pb.Cost(MeterInputUncached, subject, g.cfg.MaxInputTokenEstimate, now)
	if err != nil {
		return Money{}, err
	}
	out, err := pb.Cost(MeterOutput, subject, g.cfg.MaxOutputTokenEstimate, now)
	if err != nil {
		return Money{}, err
	}
	return in.Add(out)
}

func (g *Gate) priceBookFor(ctx context.Context, tenantID uuid.UUID) (*PriceBook, error) {
	g.mu.Lock()
	if pb, ok := g.priceBooks[tenantID]; ok {
		g.mu.Unlock()
		return pb, nil
	}
	g.mu.Unlock()

	var pb *PriceBook
	err := g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		pb, lerr = LoadPriceBook(ctx, tx)
		return lerr
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.priceBooks[tenantID] = pb
	g.mu.Unlock()
	return pb, nil
}

func (g *Gate) sessionBudgetFor(ctx context.Context, tenantID, sessionID uuid.UUID) (*Budget, error) {
	g.mu.Lock()
	if st, ok := g.sessionState[sessionID]; ok {
		g.mu.Unlock()
		return st.budget, nil
	}
	g.mu.Unlock()

	var found *Budget
	err := g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, ok, gerr := GetBudget(ctx, tx, BudgetScopeSession, &sessionID)
		if gerr != nil {
			return gerr
		}
		if ok {
			found = &b
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.sessionState[sessionID] = sessionCeilState{budget: found}
	g.mu.Unlock()
	return found, nil
}

func (g *Gate) tenantBudgetFor(ctx context.Context, tenantID uuid.UUID) (*Budget, error) {
	g.mu.Lock()
	if b, ok := g.tenantCache[tenantID]; ok {
		g.mu.Unlock()
		return b, nil
	}
	g.mu.Unlock()

	var found *Budget
	err := g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, ok, gerr := GetBudget(ctx, tx, BudgetScopeTenant, nil)
		if gerr != nil {
			return gerr
		}
		if ok {
			found = &b
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.tenantCache[tenantID] = found
	g.mu.Unlock()
	return found, nil
}

// ensureArmed initializes budgetID's Redis epoch/spend keys the first time
// this process touches it, rehydrating the spend counter from Postgres's
// own reconciled cost_records history — never from zero (README task 4.4's
// "never 'no spend yet'") — so a cold-started or previously-flushed Redis
// recovers to the TRUE spend rather than silently re-opening the ceiling.
// A no-op if the keys already exist (luaArm), so re-arming a still-live
// budget from a second process never clobbers live state.
func (g *Gate) ensureArmed(ctx context.Context, tenantID uuid.UUID, b *Budget) error {
	g.mu.Lock()
	if g.armed[b.ID] {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	var baseline int64
	err := g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var serr error
		baseline, serr = SumCostRecords(ctx, tx, BudgetScopeTenant, nil)
		return serr
	})
	if err != nil {
		return fmt.Errorf("sum existing cost records to arm budget %s: %w", b.ID, err)
	}
	if err := g.redis.Arm(ctx, b.ID, b.Epoch, baseline); err != nil {
		return err
	}
	g.mu.Lock()
	g.armed[b.ID] = true
	g.mu.Unlock()
	return nil
}

func (g *Gate) refuse(ctx context.Context, res Reservation, budgetID *uuid.UUID, reserved Money, reason string) (Reservation, error) {
	res.Decision = Decision{Kind: DecisionRefuseCeiling, Reason: reason, BudgetID: budgetID, Reserved: Money{Currency: reserved.Currency}}
	g.bestEffortPersist(ctx, res.TenantID, res.SessionID, res.Decision)
	return res, fmt.Errorf("cost: %s", reason)
}

func (g *Gate) bestEffortPersist(ctx context.Context, tenantID, sessionID uuid.UUID, d Decision) {
	if err := g.persistDecision(ctx, tenantID, sessionID, d); err != nil {
		slog.Error("cost: failed to persist budget decision", "error", err, "tenant_id", tenantID, "session_id", sessionID, "decision", d.Kind)
	}
}

func (g *Gate) persistDecision(ctx context.Context, tenantID, sessionID uuid.UUID, d Decision) error {
	return g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordDecision(ctx, tx, tenantID, sessionID, d)
	})
}

func decisionReason(kind DecisionKind, sessBudget, tenBudget *Budget) string {
	if kind == DecisionDegrade {
		return "reservation fit but crossed the soft threshold toward a configured ceiling"
	}
	var parts []string
	if sessBudget != nil {
		parts = append(parts, "session ceiling has room")
	}
	if tenBudget != nil {
		parts = append(parts, "tenant ceiling has room")
	}
	return strings.Join(parts, "; ")
}
