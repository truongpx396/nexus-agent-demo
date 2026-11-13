package kernel

import (
	"context"
	"slices"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// Pinned is the part of a run's configuration that is not the same for every
// session: the tenant's memory text and the tool schemas the run itself added (a
// per-user MCP server). A fresh run records both in its opening events
// (memory_loaded, tool_loaded), and any process that later resumes the session
// reads them back with RehydratePinned, so the model sees the system prompt and
// tool list the session was started with no matter which worker drives the
// next turn or what the memory files say by then (constitution Principle III:
// the prompt prefix never changes mid-session).
//
// It lives on RunState (RunState.Pinned) and runTurns applies it, so every
// entry into the turn loop (Run, Continue, Resume, ResumeDelegation,
// ResumeConversation) gets it without each resuming service remembering to.
// The zero value is valid and changes nothing, which is what a session started
// before these payloads carried their text sees.
type Pinned struct {
	MemoryText   string
	ExtraCatalog []provider.ToolSchema
}

// Apply returns base with the pinned memory appended to its system prompt and
// the pinned tools appended to its catalog. base is never modified: the catalog
// is copied before anything is appended, because base.Catalog is the process-wide
// slice every concurrent run shares.
func (p Pinned) Apply(base RunConfig) RunConfig {
	if p.MemoryText != "" {
		base.System = base.System + "\n\n" + p.MemoryText
	}
	if len(p.ExtraCatalog) > 0 {
		base.Catalog = append(slices.Clone(base.Catalog), p.ExtraCatalog...)
	}
	return base
}

// RehydratePinned reads a session's Pinned back from its history. history must
// be in seq order, as for Rehydrate. Only memory_loaded and tool_loaded events
// are decrypted; every other event is skipped without being opened. A tool_loaded
// event that names only a tool id is a resident tool, which every process already
// has, so it adds nothing.
func RehydratePinned(ctx context.Context, history []eventlog.Event, decrypt DecryptFunc) (Pinned, error) {
	var out Pinned
	for _, e := range history {
		if e.Type == eventlog.EventMemoryLoaded {
			var p memoryLoadedPayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return Pinned{}, err
			}
			if p.Text != "" {
				out.MemoryText = p.Text
			}
		}
		if e.Type == eventlog.EventToolLoaded {
			var p toolLoadedPayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return Pinned{}, err
			}
			if p.carriesSchema() {
				out.ExtraCatalog = append(out.ExtraCatalog, provider.ToolSchema{Name: p.ToolID, Description: p.Description, InputSchema: p.InputSchema})
			}
		}
	}
	return out, nil
}

// carriesSchema reports whether the payload records a tool the run added, as
// opposed to a resident one named by id alone.
func (p toolLoadedPayload) carriesSchema() bool {
	return p.Description != "" || len(p.InputSchema) > 0
}

// extraTools returns the schemas of cfg.LoadedTools that cfg.ExtraCatalog
// supplies, keyed by tool id. These are the tools a fresh run's tool_loaded
// events record with their schema.
func (cfg RunConfig) extraTools() map[string]provider.ToolSchema {
	if len(cfg.ExtraCatalog) == 0 {
		return nil
	}
	out := make(map[string]provider.ToolSchema, len(cfg.ExtraCatalog))
	for _, s := range cfg.ExtraCatalog {
		if slices.Contains(cfg.LoadedTools, s.Name) {
			out[s.Name] = s
		}
	}
	return out
}

// toolLoadedFor is the payload recorded for one entry of cfg.LoadedTools: id
// only for a resident tool, id and schema for one the run added.
func toolLoadedFor(id string, extras map[string]provider.ToolSchema) toolLoadedPayload {
	p := toolLoadedPayload{ToolID: id}
	if s, ok := extras[id]; ok {
		p.Description, p.InputSchema = s.Description, s.InputSchema
	}
	return p
}

// pinnedFor is the Pinned a fresh run's own opening events record, in the order
// they are written, so Run (which seeds and then enters the loop in one call)
// shows the model exactly what RehydratePinned will later read back from its log.
func (cfg RunConfig) pinnedFor() Pinned {
	p := Pinned{MemoryText: cfg.MemoryText}
	extras := cfg.extraTools()
	for _, id := range cfg.LoadedTools {
		if rec := toolLoadedFor(id, extras); rec.carriesSchema() {
			p.ExtraCatalog = append(p.ExtraCatalog, provider.ToolSchema{Name: id, Description: rec.Description, InputSchema: rec.InputSchema})
		}
	}
	return p
}
