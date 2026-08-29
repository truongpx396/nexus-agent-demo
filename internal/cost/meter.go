package cost

import "fmt"

// MeterID names one billable dimension. The token family below is the only
// family Reserve/Reconcile actually emit against this phase; the
// non-reservable examples exist to prove the seam task 4.2 calls for
// ("non-token meters registered but unemitted") without inventing a call
// site that has nothing real to measure yet.
type MeterID string

const (
	// MeterInputUncached, MeterInputCacheRead, MeterInputCacheWrite, and
	// MeterOutput mirror internal/provider.Usage's four token classes
	// exactly (provider.go: "usage split by token class ... the >90%
	// cache-read target unmeasurable" otherwise) — Reconcile prices each
	// one separately rather than collapsing them into one undifferentiated
	// token count.
	MeterInputUncached   MeterID = "input_uncached"
	MeterInputCacheRead  MeterID = "input_cache_read"
	MeterInputCacheWrite MeterID = "input_cache_write"
	MeterOutput          MeterID = "output"

	// MeterSandboxSeconds and MeterToolInvocations are registered but never
	// emitted this phase (task 4.2's own wording) — Phase 5's sandbox and
	// Phase 3's tool pipeline are real, but neither is wired to Record a
	// meter reading yet; that wiring is future work, not a Phase 4 task.
	// They exist in the registry now so a future call site has a meter to
	// Record against without also needing a schema/registry change.
	MeterSandboxSeconds  MeterID = "sandbox_seconds"
	MeterToolInvocations MeterID = "tool_invocations"

	// MeterUnreportedReservation is not a real usage dimension — it is
	// never Reserved against and never appears in DefaultMeters(). Reconcile
	// (gate.go) uses it as the cost_records label for the one case task 4.7
	// describes: a stream failed after the commit point with no usable
	// usage figures, so Reconcile charges the full reserved worst case
	// instead of trusting a partial/zero usage chunk — "an unreliable
	// provider must not look free."
	MeterUnreportedReservation MeterID = "unreported_reservation"
)

// Meter describes one registered billable dimension: its unit (for display)
// and whether BudgetGate.Reserve ever reserves against it ahead of time —
// the token family is Reservable (a pre-spend estimate is possible and
// required); sandbox/tool meters are not (their cost is only known after
// the fact, so they can only ever be Recorded, never Reserved).
type Meter struct {
	ID         MeterID
	Unit       string
	Reservable bool
}

// Registry is the set of meters this process knows about. Construct once
// via DefaultMeters and share it — it is immutable after construction.
type Registry struct {
	meters map[MeterID]Meter
}

// DefaultMeters returns the registry every Gate is constructed with: the
// four reservable token meters, plus the non-reservable examples task 4.2
// asks for.
func DefaultMeters() *Registry {
	r := &Registry{meters: map[MeterID]Meter{}}
	for _, m := range []Meter{
		{ID: MeterInputUncached, Unit: "tokens", Reservable: true},
		{ID: MeterInputCacheRead, Unit: "tokens", Reservable: true},
		{ID: MeterInputCacheWrite, Unit: "tokens", Reservable: true},
		{ID: MeterOutput, Unit: "tokens", Reservable: true},
		{ID: MeterSandboxSeconds, Unit: "seconds", Reservable: false},
		{ID: MeterToolInvocations, Unit: "count", Reservable: false},
	} {
		r.meters[m.ID] = m
	}
	return r
}

// Lookup returns the registered Meter for id, or false if it was never
// registered — a caller pricing or recording an unknown meter has a bug,
// not a data condition to paper over.
func (r *Registry) Lookup(id MeterID) (Meter, bool) {
	m, ok := r.meters[id]
	return m, ok
}

// All returns every registered meter, in a stable order (registration
// order isn't preserved by the underlying map, so this fixes one instead
// of leaving iteration order to chance for callers like a future admin
// listing).
func (r *Registry) All() []Meter {
	order := []MeterID{
		MeterInputUncached, MeterInputCacheRead, MeterInputCacheWrite, MeterOutput,
		MeterSandboxSeconds, MeterToolInvocations,
	}
	out := make([]Meter, 0, len(order))
	for _, id := range order {
		if m, ok := r.meters[id]; ok {
			out = append(out, m)
		}
	}
	return out
}

func (m MeterID) String() string { return string(m) }

// errUnknownMeter is returned by PriceBook.Cost for a meter Reconcile is
// asked to price but the registry never registered — see gate.go.
func errUnknownMeter(id MeterID) error {
	return fmt.Errorf("cost: unknown meter %q", id)
}
