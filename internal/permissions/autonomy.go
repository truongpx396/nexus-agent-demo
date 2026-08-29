package permissions

import "fmt"

// AutonomyLevel is layer 3 of the chain: how much a session may do without a
// human in the loop. Ordered loosest-last so "widen" and "tighten" are
// simple integer comparisons.
type AutonomyLevel int

const (
	AutonomyReadOnly AutonomyLevel = iota
	AutonomySupervised
	AutonomyAutonomous
)

func (l AutonomyLevel) String() string {
	switch l {
	case AutonomyReadOnly:
		return "read_only"
	case AutonomySupervised:
		return "supervised"
	case AutonomyAutonomous:
		return "autonomous"
	default:
		return fmt.Sprintf("autonomy_level(%d)", int(l))
	}
}

// ParseAutonomyLevel parses the session column's stored string form.
// Unrecognized input fails closed to the strictest level rather than
// erroring the caller into an undefined default.
func ParseAutonomyLevel(s string) (AutonomyLevel, error) {
	switch s {
	case "read_only":
		return AutonomyReadOnly, nil
	case "supervised":
		return AutonomySupervised, nil
	case "autonomous":
		return AutonomyAutonomous, nil
	default:
		return AutonomyReadOnly, fmt.Errorf("permissions: unknown autonomy level %q", s)
	}
}

// Autonomy is the pinned, one-way ratchet (README task 3.7, pattern 18): a
// session's level may only ever move toward AutonomyReadOnly, never away
// from it. This type deliberately exposes no method that could increase
// level — Pin sets it once at session start, Tighten is the only mutator,
// and Tighten itself refuses a looser target. autonomy_test.go's
// TestNoWideningMethodExists asserts this by reflection so a future edit
// that adds one fails the build, not just a manual read of this file.
type Autonomy struct {
	level AutonomyLevel
}

// Pin fixes a session's autonomy level at run start. Not named "New" so a
// call site reads as what it is: a one-time pin, not a general constructor
// a later layer might call again mid-run.
func Pin(level AutonomyLevel) *Autonomy {
	return &Autonomy{level: level}
}

// Level returns the currently pinned level.
func (a *Autonomy) Level() AutonomyLevel { return a.level }

// Tighten moves the level toward AutonomyReadOnly. A target that is not
// strictly tighter than (or equal to) the current level is refused — this
// is the ratchet's only moving part, and it only ever moves one way.
func (a *Autonomy) Tighten(target AutonomyLevel) error {
	if target > a.level {
		return fmt.Errorf("permissions: cannot widen autonomy from %s to %s", a.level, target)
	}
	a.level = target
	return nil
}

// Resolve is layer 3's opinion for one invocation: read_only denies any
// non-read-only effect outright; supervised asks for anything that isn't
// read-only; autonomous defers to whatever the remaining layers decide.
func (a *Autonomy) Resolve(effectClass EffectClass) LayerOutcome {
	if effectClass == EffectClassReadOnly {
		return LayerOutcome{Decision: Defer, Reason: "read-only effect class"}
	}
	switch a.level {
	case AutonomyReadOnly:
		return LayerOutcome{Decision: Deny, Reason: fmt.Sprintf("autonomy level %s permits read-only effects only", a.level)}
	case AutonomySupervised:
		return LayerOutcome{Decision: Ask, AskKind: AskOnce, Reason: fmt.Sprintf("autonomy level %s requires approval for a %s effect", a.level, effectClass)}
	case AutonomyAutonomous:
		return LayerOutcome{Decision: Defer, Reason: "autonomy level autonomous defers non-read-only effects to the remaining layers"}
	default:
		return LayerOutcome{Decision: Deny, Reason: fmt.Sprintf("unrecognized autonomy level %s; failing closed", a.level)}
	}
}
