package kernel

import (
	"context"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/promptctx"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// reconcile calls BudgetGate.Reconcile and logs (never terminates the run
// on) a failure — Reserve's own decision-persist failure already fails
// closed BEFORE any spend is incurred (internal/cost.Gate.Reserve's doc
// comment); by the time Reconcile runs, the call has already happened, so
// a reconciliation failure means the true cost may be under-accounted,
// never that the run's own turn failed. Escalating it into a hard stop
// here would let an accounting write outage kill an otherwise-healthy run.
func (k *Kernel) reconcile(ctx context.Context, st *RunState, res cost.Reservation, usage provider.Usage, reported bool) {
	if err := k.Budget.Reconcile(ctx, res, usage, reported); err != nil {
		log.Error().Err(err).Any("session_id", st.SessionID).Any("reservation_id", res.ID).Msg("kernel: cost reconciliation failed")
	}
}

// condense runs task 7.11's metered structured-compaction call: reserve
// (cost.PurposeCompaction, the SAME Provider port every other model call
// goes through, under cfg.CondenserModelID — a cheaper model, "off the
// paying loop" per task 4.8's own wording, meaning cheaper, never
// unmetered), then stream, then reconcile — all in this one function, the
// same reserve-then-stream shape tests/contract's metering AST check
// requires of every Provider.Stream call site. Returns the reservation too,
// so the caller can still append its own EventBudgetDecision the same way
// runTurns' turn-level reserve does. degraded=true (on a reserve refusal, a
// stream/transport failure, or empty output) means the caller must apply
// promptctx.ExtractivePass instead — a condenser that can't run must never
// block or fail the turn, only degrade it.
func (k *Kernel) condense(ctx context.Context, st *RunState, cfg RunConfig, covered []provider.Message) (summary string, degraded bool, reservation cost.Reservation, err error) {
	reservation, reserveErr := k.Budget.Reserve(ctx, cost.ReserveRequest{
		TenantID: st.TenantID, SessionID: st.SessionID, ModelID: cfg.CondenserModelID, Purpose: cost.PurposeCompaction,
	})
	if reserveErr != nil {
		return "", true, reservation, nil
	}

	prompt := promptctx.CondensePrompt(covered)
	spanCtx, genSpan := k.startSpan(ctx, "model.condense", obs.ObservationGeneration, obs.Attrs{"model.id": cfg.CondenserModelID})
	stream, serr := k.Provider.Stream(spanCtx, prompt, nil, provider.RunContext{TenantID: st.TenantID, SessionID: st.SessionID})
	if serr != nil {
		genSpan.End(obs.Attrs{"outcome": "error"})
		k.reconcile(ctx, st, reservation, provider.Usage{}, false)
		return "", true, reservation, nil
	}

	var text strings.Builder
	var usage provider.Usage
	var usageReported bool
	var streamErr error
	for {
		chunk, ok, nerr := stream.Next(ctx)
		if nerr != nil {
			streamErr = nerr
			break
		}
		if !ok {
			break
		}
		switch chunk.Kind { //nolint:exhaustive // deliberately narrow: the condenser prompt asks for plain text only — reasoning/tool_use chunks are not meaningful for a summarization call, and ChunkDone carries nothing this loop needs beyond stream.Next reporting ok=false
		case provider.ChunkContent:
			text.WriteString(chunk.Text)
		case provider.ChunkUsage:
			usage = chunk.Usage
			usageReported = true
		}
	}
	outcome := "stop"
	if streamErr != nil {
		outcome = "error"
	}
	if k.TraceContent {
		genSpan.SetContent(marshalContent(prompt), text.String())
	}
	genSpan.End(obs.Attrs{
		"model.id":             cfg.CondenserModelID,
		"usage.input_uncached": strconv.Itoa(usage.InputUncached),
		"usage.output":         strconv.Itoa(usage.OutputTokens),
		"outcome":              outcome,
	})
	k.reconcile(ctx, st, reservation, usage, usageReported && streamErr == nil)

	if streamErr != nil || text.Len() == 0 {
		return "", true, reservation, nil
	}
	return text.String(), false, reservation, nil
}
