package provider

import "testing"

func TestRouteKnownCombinations(t *testing.T) {
	labels := []DataLabel{DataLabelPublic, DataLabelInternal, DataLabelConfidential, DataLabelRestricted}
	difficulties := []Difficulty{DifficultySimple, DifficultyComplex}

	for _, l := range labels {
		for _, d := range difficulties {
			t.Run(string(l)+"/"+string(d), func(t *testing.T) {
				got := Route(l, d)
				if got.ModelID == "" {
					t.Fatalf("Route(%s, %s) returned an empty model id", l, d)
				}
				if got.Reason["data_label"] != string(l) || got.Reason["difficulty"] != string(d) {
					t.Fatalf("Route(%s, %s) reason does not name the inputs it decided on: %+v", l, d, got.Reason)
				}
				if _, fellBack := got.Reason["fallback"]; fellBack {
					t.Fatalf("Route(%s, %s) is a known combination but fell back: %+v", l, d, got.Reason)
				}
			})
		}
	}
}

func TestRouteDeterministic(t *testing.T) {
	a := Route(DataLabelConfidential, DifficultyComplex)
	b := Route(DataLabelConfidential, DifficultyComplex)
	if a.ModelID != b.ModelID {
		t.Fatalf("Route is not deterministic: %q != %q", a.ModelID, b.ModelID)
	}
}

func TestRouteUnknownLabelFallsBackConservatively(t *testing.T) {
	got := Route(DataLabel("nonexistent"), DifficultySimple)
	if got.Reason["fallback"] == "" {
		t.Fatalf("expected a named fallback reason, got %+v", got.Reason)
	}
	if got.ModelID != routeTable[DataLabelConfidential][DifficultyComplex] {
		t.Fatalf("unknown label should fall back to the most conservative tier, got %q", got.ModelID)
	}
}

func TestRouteUnknownDifficultyFallsBackToComplex(t *testing.T) {
	got := Route(DataLabelPublic, Difficulty("nonexistent"))
	if got.Reason["fallback"] == "" {
		t.Fatalf("expected a named fallback reason, got %+v", got.Reason)
	}
	if got.ModelID != routeTable[DataLabelPublic][DifficultyComplex] {
		t.Fatalf("unknown difficulty should fall back to the complex tier, got %q", got.ModelID)
	}
}
