package provider

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

var wireNameRule = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func TestSanitizeWireName(t *testing.T) {
	long := "mcp/some-very-long-server-name/some_extraordinarily_long_tool_name_that_goes_on@v1"
	if len(long) <= maxWireNameLen {
		t.Fatalf("fixture must exceed %d bytes, has %d", maxWireNameLen, len(long))
	}
	cases := []struct {
		name, ref, want string
	}{
		{"builtin", "platform/shell@v1", "platform_shell_v1"},
		{"multi segment", "mcp/github/list_prs@v1", "mcp_github_list_prs_v1"},
		{"dotted version", "platform/web_fetch@v1.2.3", "platform_web_fetch_v1_2_3"},
		{"already safe", "get_weather", "get_weather"},
		{"hyphen kept", "do-thing", "do-thing"},
		{"non ascii", "é/x", "___x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeWireName(tc.ref); got != tc.want {
				t.Fatalf("sanitizeWireName(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}

	t.Run("over 64 bytes is shortened with a digest suffix", func(t *testing.T) {
		got := sanitizeWireName(long)
		if len(got) != maxWireNameLen {
			t.Fatalf("len = %d, want %d (%q)", len(got), maxWireNameLen, got)
		}
		if !wireNameRule.MatchString(got) {
			t.Fatalf("%q does not match the wire rule", got)
		}
		if want := "mcp_some-very-long-server-name_some_extraordinarily_lon_"; got[:maxWireNameLen-9+1] != want {
			t.Fatalf("prefix = %q, want %q", got[:maxWireNameLen-9+1], want)
		}
		other := sanitizeWireName(long + "x")
		if got == other {
			t.Fatal("two long references sharing a 55-byte prefix must still differ")
		}
	})
}

func TestWireNamesRoundTrip(t *testing.T) {
	catalog := []ToolSchema{
		{Name: "platform/shell@v1"},
		{Name: "platform/web_fetch@v1"},
		{Name: "mcp/github/list_prs@v1"},
		{Name: "mcp/" + strings.Repeat("a", 80) + "@v1"},
	}
	names, err := NewWireNames(catalog)
	if err != nil {
		t.Fatalf("NewWireNames: %v", err)
	}
	for _, s := range catalog {
		wire := names.Wire(s.Name)
		if !wireNameRule.MatchString(wire) {
			t.Errorf("Wire(%q) = %q, does not match %s", s.Name, wire, wireNameRule)
		}
		if back := names.Ref(wire); back != s.Name {
			t.Errorf("Ref(Wire(%q)) = %q", s.Name, back)
		}
	}
}

func TestWireNamesUnknownNames(t *testing.T) {
	names, err := NewWireNames([]ToolSchema{{Name: "platform/shell@v1"}})
	if err != nil {
		t.Fatal(err)
	}
	// A replayed call to a tool that is no longer loaded is still well-formed.
	if got := names.Wire("platform/gone@v2"); got != "platform_gone_v2" {
		t.Fatalf("Wire(unknown) = %q", got)
	}
	// A name the request never advertised comes back unchanged.
	if got := names.Ref("something_else"); got != "something_else" {
		t.Fatalf("Ref(unknown) = %q", got)
	}
	var zero WireNames
	if got := zero.Wire("a/b@v1"); got != "a_b_v1" {
		t.Fatalf("zero value Wire = %q", got)
	}
	if got := zero.Ref("a_b_v1"); got != "a_b_v1" {
		t.Fatalf("zero value Ref = %q", got)
	}
}

func TestNewWireNamesRejectsACollision(t *testing.T) {
	_, err := NewWireNames([]ToolSchema{{Name: "platform/shell@v1"}, {Name: "platform_shell_v1"}})
	if !errors.Is(err, ErrWireNameCollision) {
		t.Fatalf("err = %v, want ErrWireNameCollision", err)
	}
	for _, ref := range []string{"platform/shell@v1", "platform_shell_v1"} {
		if !strings.Contains(err.Error(), ref) {
			t.Errorf("error %q does not name %q", err, ref)
		}
	}
	// The same reference listed twice is not a collision.
	if _, err := NewWireNames([]ToolSchema{{Name: "a/b@v1"}, {Name: "a/b@v1"}}); err != nil {
		t.Fatalf("duplicate identical ref: %v", err)
	}
}
