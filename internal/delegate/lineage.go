package delegate

import "github.com/google/uuid"

// Lineage is where a session sits in the spawn tree: the thread's top-level
// session it descends from, and how many spawns below that it is. It is what the
// depth and per-run bounds are measured on (README task 8.12), and what a team
// uses to refuse a member the same depth a delegate is refused.
//
// The session aggregate does not carry it. A session nobody spawned has no
// lineage record and is its own root at depth 0, so creating a thread's first
// session needs no knowledge of delegation; whoever spawns a session on another's
// behalf records it with Child.
type Lineage struct {
	Root  uuid.UUID
	Depth int
}

// Child is the lineage of a session spawned directly by a session whose own
// lineage is l.
func (l Lineage) Child() Lineage { return Lineage{Root: l.Root, Depth: l.Depth + 1} }
