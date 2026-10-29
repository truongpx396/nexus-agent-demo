package cost

import (
	"github.com/google/uuid"
)

// BudgetScope names what a Budget's ceiling applies to. "session" backs the
// worker-local, per-run hard ceiling (README task 4.5 — "an agent that
// cannot run away" per-task); "tenant" backs the cross-session,
// cross-worker ceiling task 4.4's Redis-Lua epoch counter enforces.
type BudgetScope string

const (
	BudgetScopeTenant  BudgetScope = "tenant"
	BudgetScopeSession BudgetScope = "session"
)

// Budget is one row of migrations/0005_cost.sql's budgets table: a ceiling
// in exact currency micros, scoped either to a whole tenant or to one
// session. Epoch is the generation counter Reserve's Redis Lua script
// checks (gate.go/redis.go) — it only advances via an explicit
// administrative reset (no reset operation ships this phase; the column is
// the seam, the same "ships now, exercised later" pattern the rest of this
// codebase already uses for forward-referenced columns).
type Budget struct {
	ID       uuid.UUID
	Scope    BudgetScope
	ScopeRef *uuid.UUID // nil for BudgetScopeTenant; the session_id for BudgetScopeSession
	Ceiling  Money
	Epoch    int64
}
