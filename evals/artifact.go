package evals

import "fmt"

// ArtifactKind is what kind of governed config a per-artifact case set gates
// (README task 10.10: "each skill, tool, plan, and team roster ships its
// own versioned cases, run at its promotion/enable gate").
type ArtifactKind string

const (
	ArtifactSkill ArtifactKind = "skill"
	ArtifactTool  ArtifactKind = "tool"
	ArtifactPlan  ArtifactKind = "plan"
	ArtifactTeam  ArtifactKind = "team"
)

// ArtifactRef names one versioned artifact a case set is bound to — the
// eval-gate counterpart of internal/plan's own (plan_id, version) identity,
// generalized to the other three kinds task 10.10 names. A plan's own
// version is already an immutable, promotable identity once
// internal/plan.Lifecycle.Enable pins it (internal/plan/lifecycle.go); this
// type lets that same idea apply uniformly to a skill bundle, a tool, or a
// team roster even though none of those three packages has grown a
// lifecycle state machine of their own yet.
type ArtifactRef struct {
	Kind    ArtifactKind
	ID      string
	Version int
}

func (a ArtifactRef) String() string { return fmt.Sprintf("%s:%s@v%d", a.Kind, a.ID, a.Version) }

// matches reports whether a is the artifact want names. Version 0 on want
// means "any version" — useful for a caller that wants every case ever
// written against a skill/tool/plan/team, not just its currently-enabled
// version.
func (a ArtifactRef) matches(want ArtifactRef) bool {
	if a.Kind != want.Kind || a.ID != want.ID {
		return false
	}
	return want.Version == 0 || a.Version == want.Version
}
