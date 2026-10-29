package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
)

// conversationResumer is the one runs.Resumer telegram, zalo and email share
// (cron has no human peer to resume a conversation with). Each of them needs
// the full "every event until terminal" channel, the same
// subscribe-before-append-and-enqueue shape kernelRunStarter.StartRun uses
// (runner.go), because none has a broker/SSE mechanism of its own to fall
// back on the way REST does. The surfaces start runs through
// *kernelRunStarter directly: it already implements runs.Starter.
type conversationResumer struct {
	ctl         *runctl.Control
	redisClient *redis.Client
}

var _ runs.Resumer = conversationResumer{}

func (r conversationResumer) ResumeConversation(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (<-chan runs.Event, error) {
	return resumeConversationEvents(ctx, r.redisClient, r.ctl, tenantID, sessionID, input)
}

// adminTenantLister implements cron.TenantLister over the same
// separate admin-role connection listTenantIDs already dials — nexus_app
// (the runtime pool) is NOSUPERUSER NOBYPASSRLS and cannot make a
// cross-tenant query at all (migrations/0000_app_role.sql).
type adminTenantLister struct{}

func (adminTenantLister) ListTenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	return listTenantIDs(ctx)
}
