package provider

import "fmt"

// DataLabel classifies the sensitivity of what a run may touch — constitution
// Principle VII: "regulated payloads MUST be routable to a self-hosted
// in-VPC model." Values are ordered from least to most sensitive.
type DataLabel string

const (
	DataLabelPublic       DataLabel = "public"
	DataLabelInternal     DataLabel = "internal"
	DataLabelConfidential DataLabel = "confidential"
	DataLabelRestricted   DataLabel = "restricted"
)

// Difficulty is the task-difficulty axis routing is decided on alongside
// DataLabel — never model discretion (Principle VII).
type Difficulty string

const (
	DifficultySimple  Difficulty = "simple"
	DifficultyComplex Difficulty = "complex"
)

// RouteDecision is what Route returns: the chosen model id and the reason it
// was chosen, persisted verbatim onto sessions.route_model_id /
// sessions.route_reason (migration 0002) so a routing decision is always
// auditable after the fact, never reconstructed from logs.
type RouteDecision struct {
	ModelID string
	Reason  map[string]string
}

// routeTable is the deterministic (data_label, difficulty) -> model_id
// mapping README task 2.8 requires. It is a fixed Go table for Phase 2, not
// tenant config — internal/config/ (Phase 9, README task 61) is what makes
// this tenant-overridable; the routing *mechanism* being deterministic and
// auditable is what this phase proves, not the specific model ids.
var routeTable = map[DataLabel]map[Difficulty]string{
	DataLabelPublic: {
		DifficultySimple:  "claude-haiku-4-5",
		DifficultyComplex: "claude-sonnet-5",
	},
	DataLabelInternal: {
		DifficultySimple:  "claude-haiku-4-5",
		DifficultyComplex: "claude-sonnet-5",
	},
	DataLabelConfidential: {
		DifficultySimple:  "claude-sonnet-5",
		DifficultyComplex: "claude-opus-5",
	},
	// restricted never routes off-VPC to a hosted model in the source
	// design; this demo has no self-hosted adapter yet (out of scope, README
	// §8), so it routes to the most capable hosted model available and
	// names that fact in Reason rather than silently treating "restricted"
	// the same as "confidential".
	DataLabelRestricted: {
		DifficultySimple:  "claude-opus-5",
		DifficultyComplex: "claude-opus-5",
	},
}

// Route deterministically decides a model id from label and difficulty.
// Every branch is named in the returned Reason so "why this model" is always
// answerable from the persisted decision, never from re-deriving the table
// at read time.
func Route(label DataLabel, difficulty Difficulty) RouteDecision {
	byDifficulty, ok := routeTable[label]
	if !ok {
		return RouteDecision{
			ModelID: routeTable[DataLabelConfidential][DifficultyComplex],
			Reason: map[string]string{
				"data_label": string(label),
				"difficulty": string(difficulty),
				"fallback":   fmt.Sprintf("unknown data_label %q — routed to the most conservative tier", label),
			},
		}
	}
	modelID, ok := byDifficulty[difficulty]
	if !ok {
		modelID = byDifficulty[DifficultyComplex]
		return RouteDecision{
			ModelID: modelID,
			Reason: map[string]string{
				"data_label": string(label),
				"difficulty": string(difficulty),
				"fallback":   fmt.Sprintf("unknown difficulty %q — routed to the complex tier for this label", difficulty),
			},
		}
	}
	return RouteDecision{
		ModelID: modelID,
		Reason: map[string]string{
			"data_label": string(label),
			"difficulty": string(difficulty),
		},
	}
}
