package costtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
)

// RunCounters runs the Counters contract. newCounters must return counters
// with nothing armed; every case uses a fresh budget id, so adapters backed
// by shared storage (Redis) need no cleanup between cases.
func RunCounters(t *testing.T, newCounters func(t *testing.T) cost.Counters) {
	t.Helper()
	ctx := context.Background()

	reserve := func(t *testing.T, c cost.Counters, id uuid.UUID, epoch, amount, ceiling int64) (cost.ReserveOutcome, int64) {
		t.Helper()
		out, total, err := c.Reserve(ctx, id, epoch, amount, ceiling)
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		return out, total
	}

	t.Run("a budget that was never armed is unavailable, never zero spend", func(t *testing.T) {
		out, total := reserve(t, newCounters(t), uuid.New(), 1, 10, 1_000)
		if out != cost.ReserveUnavailable || total != 0 {
			t.Errorf("Reserve on an unarmed budget = (%d, %d), want (ReserveUnavailable, 0)", out, total)
		}
	})

	t.Run("reservations accumulate up to the ceiling and no further", func(t *testing.T) {
		c, id := newCounters(t), uuid.New()
		if err := c.Arm(ctx, id, 1, 100); err != nil {
			t.Fatalf("Arm: %v", err)
		}
		if out, total := reserve(t, c, id, 1, 50, 200); out != cost.ReserveOK || total != 150 {
			t.Errorf("first Reserve = (%d, %d), want (ReserveOK, 150): the baseline counts", out, total)
		}
		if out, total := reserve(t, c, id, 1, 100, 200); out != cost.ReserveOverCeiling || total != 150 {
			t.Errorf("over-ceiling Reserve = (%d, %d), want (ReserveOverCeiling, 150): a refusal must leave the counter untouched", out, total)
		}
		if out, total := reserve(t, c, id, 1, 50, 200); out != cost.ReserveOK || total != 200 {
			t.Errorf("Reserve landing exactly on the ceiling = (%d, %d), want (ReserveOK, 200)", out, total)
		}
	})

	t.Run("a reservation under the wrong epoch is refused as stale", func(t *testing.T) {
		c, id := newCounters(t), uuid.New()
		if err := c.Arm(ctx, id, 2, 0); err != nil {
			t.Fatalf("Arm: %v", err)
		}
		if out, total := reserve(t, c, id, 1, 10, 1_000); out != cost.ReserveStaleEpoch || total != 2 {
			t.Errorf("Reserve under epoch 1 = (%d, %d), want (ReserveStaleEpoch, 2)", out, total)
		}
	})

	t.Run("release frees headroom, and a negative release charges an overrun", func(t *testing.T) {
		c, id := newCounters(t), uuid.New()
		if err := c.Arm(ctx, id, 1, 0); err != nil {
			t.Fatalf("Arm: %v", err)
		}
		reserve(t, c, id, 1, 100, 100)
		if out, _ := reserve(t, c, id, 1, 1, 100); out != cost.ReserveOverCeiling {
			t.Fatalf("full counter admitted another unit (outcome %d)", out)
		}
		if err := c.Release(ctx, id, 1, 60); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if out, total := reserve(t, c, id, 1, 60, 100); out != cost.ReserveOK || total != 100 {
			t.Errorf("Reserve after Release(60) = (%d, %d), want (ReserveOK, 100)", out, total)
		}
		if err := c.Release(ctx, id, 1, -10); err != nil {
			t.Fatalf("Release(-10): %v", err)
		}
		if out, total := reserve(t, c, id, 1, 0, 1_000); out != cost.ReserveOK || total != 110 {
			t.Errorf("spend after a negative Release = (%d, %d), want 110", out, total)
		}
	})

	t.Run("release under a stale or unknown epoch is a no-op", func(t *testing.T) {
		c, id := newCounters(t), uuid.New()
		if err := c.Arm(ctx, id, 1, 100); err != nil {
			t.Fatalf("Arm: %v", err)
		}
		if err := c.Release(ctx, id, 9, 40); err != nil {
			t.Fatalf("Release under a stale epoch: %v", err)
		}
		if err := c.Release(ctx, uuid.New(), 1, 40); err != nil {
			t.Fatalf("Release of an unknown budget: %v", err)
		}
		if _, total := reserve(t, c, id, 1, 0, 1_000); total != 100 {
			t.Errorf("spend after no-op releases = %d, want 100", total)
		}
	})

	t.Run("arming twice never clobbers a live counter", func(t *testing.T) {
		c, id := newCounters(t), uuid.New()
		if err := c.Arm(ctx, id, 1, 100); err != nil {
			t.Fatalf("Arm: %v", err)
		}
		reserve(t, c, id, 1, 10, 1_000)
		if err := c.Arm(ctx, id, 1, 0); err != nil {
			t.Fatalf("second Arm: %v", err)
		}
		if _, total := reserve(t, c, id, 1, 0, 1_000); total != 110 {
			t.Errorf("spend after a second Arm = %d, want 110: a re-arm from another process must not reset it", total)
		}
	})
}
