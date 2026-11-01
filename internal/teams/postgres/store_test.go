package postgres

import (
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/teams"
)

// TestMarshalUnmarshalRoster and TestMarshalUnmarshalTaint are README task
// 13.8 (closing production-readiness finding F9): internal/teams shipped
// with zero _test.go files despite owning the shared task board (README
// pattern §9) and its read-time taint fold — the JSON round trip these two
// pairs perform is exactly what a board_cards/teams row's roster/taint_state
// columns depend on.
func TestMarshalUnmarshalRoster(t *testing.T) {
	roster := []teams.MemberSpec{{AgentID: "agent-1", Task: "research"}, {AgentID: "agent-2", Task: "write"}}
	b, err := marshalRoster(roster)
	if err != nil {
		t.Fatalf("marshalRoster: %v", err)
	}
	got, err := unmarshalRoster(b)
	if err != nil {
		t.Fatalf("unmarshalRoster: %v", err)
	}
	if len(got) != len(roster) {
		t.Fatalf("got %d members, want %d", len(got), len(roster))
	}
	for i := range roster {
		if got[i] != roster[i] {
			t.Errorf("member %d = %+v, want %+v", i, got[i], roster[i])
		}
	}
}

func TestUnmarshalRoster_EmptyIsNilNotError(t *testing.T) {
	got, err := unmarshalRoster(nil)
	if err != nil {
		t.Fatalf("unmarshalRoster(nil): %v", err)
	}
	if got != nil {
		t.Errorf("unmarshalRoster(nil) = %v, want nil", got)
	}
}

func TestMarshalUnmarshalTaint(t *testing.T) {
	for _, engaged := range [][3]bool{{false, false, false}, {true, false, true}, {true, true, true}} {
		b, err := marshalTaint(engaged)
		if err != nil {
			t.Fatalf("marshalTaint(%v): %v", engaged, err)
		}
		got, err := unmarshalTaint(b)
		if err != nil {
			t.Fatalf("unmarshalTaint: %v", err)
		}
		if got != engaged {
			t.Errorf("round trip = %v, want %v", got, engaged)
		}
	}
}

func TestUnmarshalTaint_EmptyIsZeroValueNotError(t *testing.T) {
	got, err := unmarshalTaint(nil)
	if err != nil {
		t.Fatalf("unmarshalTaint(nil): %v", err)
	}
	if got != ([3]bool{}) {
		t.Errorf("unmarshalTaint(nil) = %v, want the zero value", got)
	}
}
