package main

import (
	"fmt"
	"net/http"

	configpg "github.com/truongpx396/nexus-agent-demo/internal/config/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/connectors"
	connectorspg "github.com/truongpx396/nexus-agent-demo/internal/connectors/postgres"
	connectorsredis "github.com/truongpx396/nexus-agent-demo/internal/connectors/redis"
	"github.com/truongpx396/nexus-agent-demo/internal/contentaccess"
	contentaccesspg "github.com/truongpx396/nexus-agent-demo/internal/contentaccess/postgres"
	contentrest "github.com/truongpx396/nexus-agent-demo/internal/contentaccess/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	costpg "github.com/truongpx396/nexus-agent-demo/internal/cost/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	delegatepg "github.com/truongpx396/nexus-agent-demo/internal/delegate/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/memory"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	oversightpg "github.com/truongpx396/nexus-agent-demo/internal/oversight/postgres"
	oversightrest "github.com/truongpx396/nexus-agent-demo/internal/oversight/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/reliability"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	runctlpg "github.com/truongpx396/nexus-agent-demo/internal/runctl/postgres"
	runctlrest "github.com/truongpx396/nexus-agent-demo/internal/runctl/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	runspg "github.com/truongpx396/nexus-agent-demo/internal/runs/postgres"
	runsrest "github.com/truongpx396/nexus-agent-demo/internal/runs/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/skills"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/mcp"
	surfpg "github.com/truongpx396/nexus-agent-demo/internal/surfaces/postgres"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
	teamspg "github.com/truongpx396/nexus-agent-demo/internal/teams/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/kerneladapter"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// stuckDetectionWindow is internal/reliability.NewRegistry's window (README
// task 6.8): how many recent tool calls a session's own Tracker looks back
// over for a repeating cycle. 8 is small enough to catch a tight retry loop
// within a handful of turns without false-tripping on a normal multi-step
// task that happens to touch the same tool (e.g. several distinct
// file_read calls) more than once.
const stuckDetectionWindow = 8

// services is the feature graph built over infra: the kernel and the use cases
// the REST API, the chat surfaces and the background loops drive.
type services struct {
	kernel      *kernel.Kernel
	gate        *cost.Gate
	starter     *kernelRunStarter
	launcher    *runs.Service
	approvals   *oversight.Approvals
	resumer     *oversight.Resumer
	grants      *contentaccess.Grants
	ctl         *runctl.Control
	delegations *delegate.Delegations
	teams       *teams.Service
	outbox      *surfaces.Outbox

	cfgStore              *configpg.Store
	skillBundles          []skills.SkillBundle
	mcp                   *mcp.Port
	catalogManifestDigest []byte
}

// buildServices constructs every shared dependency before the thing that needs
// it, in the order the wiring requires: the vault and the cost gate ahead of the
// tool pipeline (the connector and retrieval tools need them), the pipeline ahead
// of the kernel, and the kernel ahead of the use cases that run it.
func buildServices(in *infra, prov provider.Provider, spanExp spanExporter) (*services, error) {
	st, keyStore, chain, redisClient := in.st, in.keys, in.chain, in.redis

	cfgStore := configpg.NewStore(st)
	vault := &connectors.Vault{
		Config: cfgStore, Connections: connectorspg.NewConnections(st, keyStore), States: connectorsredis.NewStates(redisClient),
		Providers: newConnectorRegistry(),
	}

	// A Retriever needs a Gate to reserve embedding calls against (README task
	// 12.4), and the platform/retrieve tool needs a Retriever wired into the
	// resident catalog, so the gate exists before the pipeline does.
	gate := newCostGate(st, redisClient)

	delegations := delegate.NewDelegations(delegatepg.New(st, keyStore, chain))
	teamsSvc := teams.NewService(teamspg.New(st, keyStore, chain, teamspg.Lineage{Of: delegatepg.LineageOf, Record: delegatepg.RecordLineage}))
	stack, err := newToolPipeline(st, cfgStore, keyStore, chain, delegations, teamsSvc, vault, gate)
	if err != nil {
		return nil, fmt.Errorf("build tool pipeline: %w", err)
	}
	pipeline, catalog, catalogManifestDigest := stack.pipeline, stack.catalog, stack.manifestDigest
	loadedTools := make([]string, len(catalog))
	for i, c := range catalog {
		loadedTools[i] = c.Name
	}

	oversightStore := oversightpg.New(st, keyStore, chain)
	approvals := oversight.NewApprovals(oversightStore)
	inputs := oversight.NewInputs(oversightStore)
	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{prov}),
		Tools:    kerneladapter.PipelineExecutor{Pipeline: pipeline}, // real tool pipeline, Phase 3
		Budget:   gate,                                               // real reserve-then-reconcile cost gate, Phase 4
		Log: postgres.New(st, postgres.Hooks{
			Chain:    chain,                       // hash-chained audit receipts, Phase 5 task 5.2
			Suspend:  onSuspendFunc(approvals),    // durably record an approval on every suspend, Phase 5 task 5.6
			Delegate: onDelegateFunc(delegations), // bind a delegation to its gating tool_use, Phase 8 task 8.10
		}),
		Stuck:        reliability.NewRegistry(stuckDetectionWindow),   // README task 6.8
		Tracer:       spanExp,                                         // per-run/per-turn/per-tool-call spans, docs/local-llm.md
		TraceContent: envOr("NEXUS_TRACE_CONTENT", "false") == "true", // opt-in prompt/tool content in traces, docs/local-llm.md — off by default, keeps every span content-free unless explicitly requested
	}

	starter := &kernelRunStarter{
		kernel:      k,
		system:      "You are a helpful agent. Tools may be denied or require approval depending on the session's autonomy level.",
		catalog:     catalog,
		loadedTools: loadedTools,
		maxTurns:    25,
		memory:      &memory.Store{RootDir: envOr("NEXUS_MEMORY_ROOT", ".dev/memory"), Config: cfgStore},
		queue:       in.queue,
		redisClient: redisClient,
	}

	// Wire must land before the first real dispatch. It reuses starter's own
	// System/MaxTurns so a delegated child's kernel run is configured identically
	// to a root run's (README task 8.9's own child session inherits the parent's
	// harness_digest for the same reason).
	delegations.Wire(delegate.Config{
		Prices: costpg.New(st),
		Kernel: k, Pipeline: pipeline,
		System: starter.system, Catalog: catalog, LoadedTools: loadedTools, MaxTurns: starter.maxTurns,
	})

	resumer := &oversight.Resumer{
		Kernel: k, Approvals: approvals, Sessions: oversightStore, Keys: oversightStore,
		System: starter.system, Catalog: catalog, MaxTurns: starter.maxTurns,
	}
	grants := contentaccess.NewGrants(contentaccesspg.NewGrantStore(st, keyStore, chain))

	ctl := &runctl.Control{
		Store: runctlpg.New(st, keyStore, chain), Approvals: approvals, Inputs: inputs, Kernel: k,
		System: starter.system, Catalog: catalog, MaxTurns: starter.maxTurns, CatalogManifestDigest: catalogManifestDigest,
		Queue: in.queue,
		// Who is told a session settled: a delegation's parent, a team.
		Observers: []runctl.SettledObserver{delegations, teamsSvc},
	}

	// Canceler is ctl itself: endTeam reaps a still-active member the same way
	// runctl.Control.Cancel already reaps anything else (README task 9.9, reusing
	// 8.14). Like delegations.Wire, it must land before the first real dispatch.
	teamsSvc.Wire(teams.Config{
		Prices: costpg.New(st),
		Kernel: k, Pipeline: pipeline, Canceler: ctl,
		System: starter.system, Catalog: catalog, LoadedTools: loadedTools, MaxTurns: starter.maxTurns,
	})

	// One run-admission use case for every surface that starts a fresh run:
	// admit the session through the control plane, mint its key, start it.
	launcher := &runs.Service{
		Admit:                 newControlPlane(st, gate, chain, approvals, grants),
		Keys:                  runspg.Keys{Store: st, KeyStore: keyStore},
		Starter:               starter,
		CatalogManifestDigest: catalogManifestDigest,
		ServedModel:           func(routed string) string { return provider.ServedModel(prov, routed) },
	}

	return &services{
		kernel: k, gate: gate, starter: starter, launcher: launcher,
		approvals: approvals, resumer: resumer, grants: grants, ctl: ctl,
		delegations: delegations, teams: teamsSvc,
		outbox:   &surfaces.Outbox{Ledger: surfpg.NewLedger(st, keyStore, chain)},
		cfgStore: cfgStore, skillBundles: stack.skillBundles, mcp: stack.mcp, catalogManifestDigest: catalogManifestDigest,
	}, nil
}

// newRESTAPI builds the run API and mounts every feature's REST module on the kit
// that authenticates them. The run endpoints also own the live fan-out of a run's
// events, which the queue workers and the kernel's chunk callback publish into, so
// the handler is returned for them as well as the mounted http.Handler. It points
// the kernel's chunk callback at that fan-out.
func newRESTAPI(in *infra, svc *services, spanExp spanExporter) (*runsrest.Handler, http.Handler) {
	runsAPI := runsrest.NewHandler(svc.launcher, runspg.Reader{Store: in.st, KeyStore: in.keys})
	// Redis Pub/Sub-backed EventBus (broker.go's own doc comment on why this
	// surface needs one now): every publish reaches every nexusd process, not just
	// whichever one produced it — required once a run's turn loop may execute on a
	// different process (internal/queue's worker pool) than whichever process holds
	// the client's own SSE connection.
	runsAPI.Bus = redisEventBus{client: in.redis}
	runsAPI.Skills = &nexusdSkillSetPort{config: svc.cfgStore, bundles: svc.skillBundles}
	runsAPI.MCP = svc.mcp
	runsAPI.Outbox = svc.outbox
	runsAPI.OutboxSender = logSender{}
	runsAPI.Exporter = spanExp
	svc.kernel.OnChunk = publishDeltas(runsAPI)

	srv := restkit.NewServer(in.verifier,
		runsAPI,
		oversightrest.Handler{Approvals: svc.approvals, Decisions: oversight.Decisions{Resume: svc.resumer, Settled: svc.ctl}},
		contentrest.Handler{Grants: svc.grants},
		runctlrest.Handler{Control: svc.ctl},
	)
	return runsAPI, srv.Handler()
}

// publishDeltas is the kernel's chunk callback: a live typing-style preview over
// SSE, decoupled from the durable event path. It fires as the current turn's
// provider stream decodes each chunk (kernel/turns.go), well before that turn's
// own accumulation/classification/append; runsAPI.PublishDelta fans it out to
// whichever clients are subscribed to that session's /v1/runs/{id}/events stream
// right now and drops it silently for anyone who isn't (the same non-blocking,
// best-effort delivery the durable path's own broker.publish has). Nothing about
// cost reconcile, tool dispatch, or the audit chain reads this — those still only
// ever see the complete, accumulated turn.
func publishDeltas(runsAPI *runsrest.Handler) func(kernel.ChunkEvent) {
	return func(ev kernel.ChunkEvent) {
		switch ev.Chunk.Kind {
		case provider.ChunkContent:
			runsAPI.PublishDelta(ev.SessionID, runsrest.DeltaDTO{Kind: "content", Text: ev.Chunk.Text})
		case provider.ChunkToolUse:
			runsAPI.PublishDelta(ev.SessionID, runsrest.DeltaDTO{Kind: "tool_use", ToolUseID: ev.Chunk.ToolUseID, ToolName: ev.Chunk.ToolName})
		case provider.ChunkReasoning:
			// No Text/Opaque on this signal at all (kernel.Kernel.OnChunk's own doc
			// comment already strips it before this closure ever sees it) — a client
			// learns only that the model is reasoning right now, the same "event
			// visible, body redacted" shape EventThought's own durable record has.
			runsAPI.PublishDelta(ev.SessionID, runsrest.DeltaDTO{Kind: "reasoning"})
		case provider.ChunkUsage, provider.ChunkDone:
			// Turn-level bookkeeping a client has no use for as a live preview — the
			// durable EventTerminal/cost records still carry them.
		}
	}
}
