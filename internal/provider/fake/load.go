package fake

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadScripts reads every *.yaml file in dir (in filename order, for
// determinism) and unmarshals each into a Script, so a corpus of scripted
// provider turns can live as reviewable files rather than Go literals.
func LoadScripts(dir fs.FS) ([]Script, error) {
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return nil, fmt.Errorf("read scripts dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	scripts := make([]Script, 0, len(names))
	for _, name := range names {
		raw, err := fs.ReadFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("read script %s: %w", name, err)
		}
		var s Script
		if err := yaml.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("parse script %s: %w", name, err)
		}
		scripts = append(scripts, s)
	}
	return scripts, nil
}
