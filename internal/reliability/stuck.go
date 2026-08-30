package reliability

import (
	"crypto/sha256"
	"sync"

	"github.com/google/uuid"
)

// StuckReason names which of task 6.8's three patterns a Tracker detected.
// All three reduce to the SAME underlying check — the smallest period that
// exactly tiles a rolling window of (tool, input-digest) actions — because a
// content-free, structural-only detector (kernel.Hygiene's own discipline:
// never inspect what a tool call actually returned) has no way to tell
// "still making progress" from "back where it started" except by noticing
// the session keeps re-issuing calls it has already issued: period 1 is the
// same call every time, period 2 is a strict two-call alternation, and any
// longer period up to half the window is a longer cycle that still never
// leaves the set of states it's already visited.
type StuckReason string

const (
	ReasonRepeatedAction StuckReason = "repeated_action"
	ReasonOscillation    StuckReason = "oscillation"
	ReasonZeroNetChange  StuckReason = "zero_net_change"
)

// action is one turn's structural signal: which tool, and a digest of its
// input — never the input itself, matching Hygiene's "never inspect
// content" discipline and keeping the rolling window cheap to hold in
// memory for the life of a session.
type action struct {
	tool   string
	digest [32]byte
}

// Digest hashes a tool call's raw input into the fixed-size token Observe
// compares by equality — sha256, not a security digest here (no attacker
// controls what two calls "look the same" means), just a cheap, collision-
// safe-enough way to avoid holding full inputs in the rolling window.
func Digest(toolName string, input []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(toolName))
	h.Write([]byte{0}) // separator: never let ("ab","c") collide with ("a","bc")
	h.Write(input)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Verdict is what one Detector.Observe call reports.
type Verdict struct {
	Suspected bool
	Reason    StuckReason
	Period    int // the detected cycle length, 0 if not Suspected
}

// Detector looks for a repeating cycle within the last `window` actions —
// task 6.8's own three named patterns, unified as described above. window
// must be even and at least 2 for a period-2 (oscillation) pattern to ever
// be checkable; NewDetector enforces a sane floor.
type Detector struct {
	window int
}

func NewDetector(window int) *Detector {
	if window < 4 {
		window = 4
	}
	return &Detector{window: window}
}

// Observe returns the verdict for the LAST d.window actions in history
// (older entries are ignored) — Suspected only once a full window's worth
// of actions is available (an under-full history is never enough evidence).
func (d *Detector) Observe(history []action) Verdict {
	if len(history) < d.window {
		return Verdict{}
	}
	w := history[len(history)-d.window:]
	for period := 1; period <= d.window/2; period++ {
		if d.window%period != 0 {
			continue // a period must evenly tile the whole window, or it isn't a clean cycle
		}
		if tiles(w, period) {
			reason := ReasonZeroNetChange
			switch period {
			case 1:
				reason = ReasonRepeatedAction
			case 2:
				reason = ReasonOscillation
			}
			return Verdict{Suspected: true, Reason: reason, Period: period}
		}
	}
	return Verdict{}
}

func tiles(w []action, period int) bool {
	for i := period; i < len(w); i++ {
		if w[i].tool != w[i-period].tool || w[i].digest != w[i-period].digest {
			return false
		}
	}
	return true
}

// Tracker is the per-session stateful wrapper Record folds one more action
// into: task 6.8's own requirement — "non-terminal" on the first suspected
// trip, terminate only on a SECOND corroborating one — is state a bare
// Detector (pure, stateless) cannot hold by itself.
type Tracker struct {
	det              *Detector
	history          []action
	maxHistory       int
	consecutiveTrips int
	lastReason       StuckReason
}

func newTracker(window int) *Tracker {
	return &Tracker{det: NewDetector(window), maxHistory: window * 4}
}

// TrackerVerdict adds Terminate to Verdict: true only on the second
// consecutive suspected trip for the SAME reason — a suspected trip for a
// DIFFERENT reason than the last one restarts the corroboration count,
// exactly like CircuitBreaker only escalates on a run of IDENTICAL
// failures.
type TrackerVerdict struct {
	Verdict
	Terminate bool
}

func (t *Tracker) Record(toolName string, input []byte) TrackerVerdict {
	t.history = append(t.history, action{tool: toolName, digest: Digest(toolName, input)})
	if len(t.history) > t.maxHistory {
		t.history = t.history[len(t.history)-t.maxHistory:]
	}

	v := t.det.Observe(t.history)
	if !v.Suspected {
		t.consecutiveTrips = 0
		t.lastReason = ""
		return TrackerVerdict{}
	}
	if v.Reason == t.lastReason {
		t.consecutiveTrips++
	} else {
		t.lastReason = v.Reason
		t.consecutiveTrips = 1
	}
	return TrackerVerdict{Verdict: v, Terminate: t.consecutiveTrips >= 2}
}

// Registry hands out one Tracker per session, mirroring
// internal/cost.Gate's own per-session lazy-map cache — a Kernel is
// constructed once per PROCESS (cmd/nexusd's serve()) and shared across
// every session it runs, so per-session stuck-detection state cannot live
// on the Tracker itself.
type Registry struct {
	window int
	mu     sync.Mutex
	byID   map[uuid.UUID]*Tracker
}

func NewRegistry(window int) *Registry {
	return &Registry{window: window, byID: map[uuid.UUID]*Tracker{}}
}

// Record folds one more (toolName, input) action into sessionID's own
// tracker, creating it lazily on first use.
func (r *Registry) Record(sessionID uuid.UUID, toolName string, input []byte) TrackerVerdict {
	r.mu.Lock()
	tr, ok := r.byID[sessionID]
	if !ok {
		tr = newTracker(r.window)
		r.byID[sessionID] = tr
	}
	r.mu.Unlock()
	return tr.Record(toolName, input)
}

// Forget drops sessionID's tracker — called once a run reaches ANY terminal
// state, so a long-lived process's Registry doesn't accumulate one entry
// per session forever.
func (r *Registry) Forget(sessionID uuid.UUID) {
	r.mu.Lock()
	delete(r.byID, sessionID)
	r.mu.Unlock()
}
