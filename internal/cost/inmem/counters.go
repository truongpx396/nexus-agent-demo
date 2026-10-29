package inmem

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
)

// Counters is a cost.Counters under one mutex, which gives each operation the
// atomicity the Redis adapter gets from a Lua script.
type Counters struct {
	mu      sync.Mutex
	budgets map[uuid.UUID]*counter
	failErr error
}

type counter struct {
	epoch int64
	spend int64
}

var _ cost.Counters = (*Counters)(nil)

// NewCounters returns counters with nothing armed — exactly a cold Redis.
func NewCounters() *Counters { return &Counters{budgets: map[uuid.UUID]*counter{}} }

// Flush forgets every budget, as a Redis flush would: the next Reserve is
// ReserveUnavailable until the Gate re-arms from the Ledger.
func (c *Counters) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.budgets = map[uuid.UUID]*counter{}
}

// FailWith makes every later call return err, standing in for an
// unreachable Redis; nil clears it.
func (c *Counters) FailWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failErr = err
}

// Spend is the budget's current counter, and false if it was never armed.
func (c *Counters) Spend(budgetID uuid.UUID) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.budgets[budgetID]
	if !ok {
		return 0, false
	}
	return b.spend, true
}

func (c *Counters) Reserve(_ context.Context, budgetID uuid.UUID, epoch, amount, ceiling int64) (cost.ReserveOutcome, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failErr != nil {
		return cost.ReserveUnavailable, 0, c.failErr
	}
	b, ok := c.budgets[budgetID]
	if !ok {
		return cost.ReserveUnavailable, 0, nil
	}
	if b.epoch != epoch {
		return cost.ReserveStaleEpoch, b.epoch, nil
	}
	if b.spend+amount > ceiling {
		return cost.ReserveOverCeiling, b.spend, nil
	}
	b.spend += amount
	return cost.ReserveOK, b.spend, nil
}

func (c *Counters) Release(_ context.Context, budgetID uuid.UUID, epoch, delta int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failErr != nil {
		return c.failErr
	}
	if b, ok := c.budgets[budgetID]; ok && b.epoch == epoch {
		b.spend -= delta
	}
	return nil
}

func (c *Counters) Arm(_ context.Context, budgetID uuid.UUID, epoch, baseline int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failErr != nil {
		return c.failErr
	}
	if _, ok := c.budgets[budgetID]; !ok {
		c.budgets[budgetID] = &counter{epoch: epoch, spend: baseline}
	}
	return nil
}
