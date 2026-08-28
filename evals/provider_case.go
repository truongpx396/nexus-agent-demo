package evals

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
)

// ProviderScriptCase grades the deterministic fake provider itself: given a
// scripted stream, does draining it produce the expected concatenated
// content and terminal reason (or, for a deliberately broken script, the
// expected error)? It reuses fake.Script directly rather than redefining
// the chunk shape, so a corpus file and a Go-literal test fixture are the
// same type.
type ProviderScriptCase struct {
	ID          string      `yaml:"id"`
	Class       Class       `yaml:"class"`
	Description string      `yaml:"description"`
	Script      fake.Script `yaml:"script"`
	Want        WantSpec    `yaml:"want"`
}

type WantSpec struct {
	FinalText   string `yaml:"final_text"`
	Done        string `yaml:"done"`
	ExpectError bool   `yaml:"expect_error"`
}

// LoadProviderScriptCases reads every *.yaml file in dir, in filename
// order, into a ProviderScriptCase.
func LoadProviderScriptCases(dir fs.FS) ([]ProviderScriptCase, error) {
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return nil, fmt.Errorf("read corpus dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	cases := make([]ProviderScriptCase, 0, len(names))
	for _, name := range names {
		raw, err := fs.ReadFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("read case %s: %w", name, err)
		}
		var c ProviderScriptCase
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("parse case %s: %w", name, err)
		}
		cases = append(cases, c)
	}
	return cases, nil
}
