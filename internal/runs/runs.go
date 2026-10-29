// Package runs is the application layer between the surfaces and whatever
// executes a run: the vocabulary every surface speaks when it starts or
// resumes one, and the two ports (Starter, Resumer) cmd/nexusd satisfies with
// the kernel.
//
// It exists so each surface (REST, Telegram, Zalo, email, cron) stops
// declaring its own structurally-identical copy of these types. They used to,
// on the reasoning that surfaces must not import each other; a package that
// none of them is, and that imports neither the kernel nor any surface, is
// the shared dependency that reasoning was missing. This package must stay
// that way — tests/contract forbids surfaces from reaching the kernel
// transitively, so runs may not import it.
package runs

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// SealFunc seals one event's plaintext payload: the sealed bytes, a digest
// over the plaintext (it survives crypto-shredding), and the id of the key it
// was sealed under. cmd/nexusd converts it to kernel.SealFunc, a structurally
// identical type, without this package naming the kernel.
type SealFunc func(plaintext []byte) (sealed, digest []byte, keyID string, err error)

// SealFor returns the SealFunc for events appended to one session under dek.
// The tenant and session ids are bound into every ciphertext as associated
// data, so an event sealed for one session cannot be opened as another's.
func SealFor(dek crypto.DEK, tenantID, sessionID uuid.UUID) SealFunc {
	return func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("seal event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}
}

// Request is everything Starter.StartRun needs to begin one run. The session
// row has already been durably created before it is built. A surface leaves
// the fields it has no use for at their zero values.
type Request struct {
	SessionID     uuid.UUID
	TenantID      uuid.UUID
	Seal          SealFunc
	Input         string
	ModelID       string
	AutonomyLevel string

	// ExtraCatalog and ExtraLoadedTools are this run's own addition to the
	// process-wide tool catalog — the per-user MCP servers resolved when the
	// session was created, whose digest is already folded into the session's
	// harness_digest. Zero for every surface that attaches no MCP servers.
	ExtraCatalog     []provider.ToolSchema
	ExtraLoadedTools []string

	// Conversational opts the run into pause-not-terminate semantics
	// (kernel.RunConfig.Conversational): a plain reply suspends the session
	// awaiting the next message instead of ending the run.
	Conversational bool
}

// Event is one item on the channel a Starter or Resumer returns. It mirrors
// the (event, error) pair kernel.Kernel.Run yields, so a surface can relay a
// run without naming the kernel.
type Event struct {
	Event eventlog.Event
	Err   error
}

// Starter begins a run. cmd/nexusd's kernelRunStarter is the only
// implementation: the one place a kernel.RunState/RunConfig is built.
type Starter interface {
	// StartRun begins a run in the background (a real implementation returns
	// once the run is accepted, not once it completes) and returns a channel
	// of every event the run produces, in order, closed when the run ends.
	StartRun(ctx context.Context, req Request) (<-chan Event, error)
}

// Resumer continues an existing conversational session with its next message.
// The session is rehydrated from its durable log, so the caller supplies only
// identifiers and the new input.
type Resumer interface {
	ResumeConversation(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (<-chan Event, error)
}
