package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/cron"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/email"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/telegram"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/zalo"
)

// telegramStarterAdapter/zaloStarterAdapter/emailStarterAdapter/
// cronStarterAdapter translate each surface's own structurally-identical-
// but-distinct RunRequest/RunEvent/RunStarter (starter.go's own doc
// comment on why they're duplicated per surface, not shared) into a call
// against the SAME single *kernelRunStarter every surface ultimately
// shares — this file is the only place that needs to know all five
// surfaces' local types at once, matching kernelRunStarter's own doc
// comment ("the only place a kernel.RunState/kernel.RunConfig gets built").
//
// telegram/zalo/email's own adapters also implement that surface's local
// Resumer interface (ctl, wired only on those three — never cron, which
// has no human peer to resume a conversation with) — the same
// iter.Seq2-to-channel adaptation nexusdRunCtlPort.ResumeConversation
// (ports.go) already does for REST, just retargeted at each surface's own
// RunEvent type.

type telegramStarterAdapter struct {
	k   *kernelRunStarter
	ctl *runctl.Control
}

func (a telegramStarterAdapter) StartRun(ctx context.Context, req telegram.RunRequest) (<-chan telegram.RunEvent, error) {
	events, err := a.k.StartRun(ctx, rest.RunRequest{
		SessionID: req.SessionID, TenantID: req.TenantID, Seal: rest.SealFunc(req.Seal),
		Input: req.Input, ModelID: req.ModelID, AutonomyLevel: req.AutonomyLevel,
	})
	if err != nil {
		return nil, err
	}
	out := make(chan telegram.RunEvent, 8)
	go func() {
		defer close(out)
		for ev := range events {
			out <- telegram.RunEvent{Event: ev.Event, Err: ev.Err}
		}
	}()
	return out, nil
}

func (a telegramStarterAdapter) ResumeConversation(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (<-chan telegram.RunEvent, error) {
	events, err := a.ctl.ResumeConversation(ctx, tenantID, sessionID, input)
	if err != nil {
		return nil, err
	}
	out := make(chan telegram.RunEvent, 8)
	go func() {
		defer close(out)
		for ev, err := range events {
			out <- telegram.RunEvent{Event: ev, Err: err}
			if err != nil {
				return
			}
		}
	}()
	return out, nil
}

type zaloStarterAdapter struct {
	k   *kernelRunStarter
	ctl *runctl.Control
}

func (a zaloStarterAdapter) StartRun(ctx context.Context, req zalo.RunRequest) (<-chan zalo.RunEvent, error) {
	events, err := a.k.StartRun(ctx, rest.RunRequest{
		SessionID: req.SessionID, TenantID: req.TenantID, Seal: rest.SealFunc(req.Seal),
		Input: req.Input, ModelID: req.ModelID, AutonomyLevel: req.AutonomyLevel,
	})
	if err != nil {
		return nil, err
	}
	out := make(chan zalo.RunEvent, 8)
	go func() {
		defer close(out)
		for ev := range events {
			out <- zalo.RunEvent{Event: ev.Event, Err: ev.Err}
		}
	}()
	return out, nil
}

func (a zaloStarterAdapter) ResumeConversation(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (<-chan zalo.RunEvent, error) {
	events, err := a.ctl.ResumeConversation(ctx, tenantID, sessionID, input)
	if err != nil {
		return nil, err
	}
	out := make(chan zalo.RunEvent, 8)
	go func() {
		defer close(out)
		for ev, err := range events {
			out <- zalo.RunEvent{Event: ev, Err: err}
			if err != nil {
				return
			}
		}
	}()
	return out, nil
}

type emailStarterAdapter struct {
	k   *kernelRunStarter
	ctl *runctl.Control
}

func (a emailStarterAdapter) StartRun(ctx context.Context, req email.RunRequest) (<-chan email.RunEvent, error) {
	events, err := a.k.StartRun(ctx, rest.RunRequest{
		SessionID: req.SessionID, TenantID: req.TenantID, Seal: rest.SealFunc(req.Seal),
		Input: req.Input, ModelID: req.ModelID, AutonomyLevel: req.AutonomyLevel,
	})
	if err != nil {
		return nil, err
	}
	out := make(chan email.RunEvent, 8)
	go func() {
		defer close(out)
		for ev := range events {
			out <- email.RunEvent{Event: ev.Event, Err: ev.Err}
		}
	}()
	return out, nil
}

func (a emailStarterAdapter) ResumeConversation(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (<-chan email.RunEvent, error) {
	events, err := a.ctl.ResumeConversation(ctx, tenantID, sessionID, input)
	if err != nil {
		return nil, err
	}
	out := make(chan email.RunEvent, 8)
	go func() {
		defer close(out)
		for ev, err := range events {
			out <- email.RunEvent{Event: ev, Err: err}
			if err != nil {
				return
			}
		}
	}()
	return out, nil
}

type cronStarterAdapter struct{ k *kernelRunStarter }

func (a cronStarterAdapter) StartRun(ctx context.Context, req cron.RunRequest) (<-chan cron.RunEvent, error) {
	events, err := a.k.StartRun(ctx, rest.RunRequest{
		SessionID: req.SessionID, TenantID: req.TenantID, Seal: rest.SealFunc(req.Seal),
		Input: req.Input, ModelID: req.ModelID, AutonomyLevel: req.AutonomyLevel,
	})
	if err != nil {
		return nil, err
	}
	out := make(chan cron.RunEvent, 8)
	go func() {
		defer close(out)
		for ev := range events {
			out <- cron.RunEvent{Event: ev.Event, Err: ev.Err}
		}
	}()
	return out, nil
}

// adminTenantLister implements cron.TenantLister over the same
// separate admin-role connection listTenantIDs already dials — nexus_app
// (the runtime pool) is NOSUPERUSER NOBYPASSRLS and cannot make a
// cross-tenant query at all (migrations/0000_app_role.sql).
type adminTenantLister struct{}

func (adminTenantLister) ListTenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	return listTenantIDs(ctx)
}
