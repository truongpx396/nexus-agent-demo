package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// An Eraser with no ArtifactStore must refuse every operation before it touches
// the transaction: erasing the keys and leaving the plaintext artifacts behind
// would look like a complete erasure. The nil transaction proves the refusal
// comes first.
func TestEraser_WithoutAnArtifactStoreRefusesBeforeDoingAnything(t *testing.T) {
	ctx, e := context.Background(), Eraser{}

	if _, err := e.EraseTenant(ctx, nil, uuid.New(), "test"); !errors.Is(err, errNoArtifacts) {
		t.Errorf("EraseTenant = %v, want errNoArtifacts", err)
	}
	if _, err := e.EraseSession(ctx, nil, uuid.New(), uuid.New(), "test"); !errors.Is(err, errNoArtifacts) {
		t.Errorf("EraseSession = %v, want errNoArtifacts", err)
	}
	if _, err := e.ReconcileDerivedArtifacts(ctx, nil); !errors.Is(err, errNoArtifacts) {
		t.Errorf("ReconcileDerivedArtifacts = %v, want errNoArtifacts", err)
	}
}
