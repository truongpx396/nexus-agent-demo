package main

import (
	"github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	costpg "github.com/truongpx396/nexus-agent-demo/internal/cost/postgres"
	costredis "github.com/truongpx396/nexus-agent-demo/internal/cost/redis"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// newCostGate wires the budget gate to its two adapters: the Postgres ledger
// and the Redis counters behind the tenant-scoped ceiling. A nil client leaves
// the counters unwired, which the Gate treats as fail-closed for any tenant
// that has a budget rather than as "no spend".
func newCostGate(st *store.Store, redisClient *redis.Client) *cost.Gate {
	var counters cost.Counters
	if redisClient != nil {
		counters = costredis.New(redisClient, 0)
	}
	return cost.NewGate(costpg.New(st), counters, cost.DefaultMeters(), cost.GateConfig{})
}
