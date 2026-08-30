package skills

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// manifestFileName is the one required file in every bundle directory.
const manifestFileName = "skill.json"

// bundleManifest is skill.json's on-disk shape.
type bundleManifest struct {
	SkillID         string   `json:"skill_id"`
	Description     string   `json:"description"`
	TriggerHint     string   `json:"trigger_hint"`
	DeclaredToolIDs []string `json:"declared_tool_ids"`
	Signature       string   `json:"signature"` // base64
}

// LoadBundles reads one skill bundle per immediate subdirectory of rootDir:
// each must contain skill.json (bundleManifest) plus arbitrary reference
// files, and may contain a file literally named "script" — the bundle's own
// tool (task 7.5). A directory with no skill.json is skipped, not an error
// (an operator's stray file/README living alongside real bundles). This
// function does no scanning or signature verification of its own — the
// caller (cmd/nexusd) runs ScanBundle/VerifySignature on what comes back and
// decides whether to keep or refuse each bundle.
func LoadBundles(rootDir string) ([]SkillBundle, error) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skills: read %s: %w", rootDir, err)
	}

	var bundles []SkillBundle
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(rootDir, e.Name())
		manifestPath := filepath.Join(dir, manifestFileName)
		raw, err := os.ReadFile(manifestPath) //nolint:gosec // manifestPath is built from the operator-controlled skills root, never request input
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("skills: read %s: %w", manifestPath, err)
		}

		var m bundleManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("skills: parse %s: %w", manifestPath, err)
		}
		sig, err := base64.StdEncoding.DecodeString(m.Signature)
		if err != nil {
			return nil, fmt.Errorf("skills: decode signature in %s: %w", manifestPath, err)
		}

		b := SkillBundle{
			SkillID:         m.SkillID,
			Description:     m.Description,
			TriggerHint:     m.TriggerHint,
			DeclaredToolIDs: m.DeclaredToolIDs,
			Signature:       sig,
		}

		if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			switch rel {
			case manifestFileName:
				return nil
			case scriptFileName:
				content, err := os.ReadFile(path) //nolint:gosec // path walks the operator-controlled skills root, never request input
				if err != nil {
					return fmt.Errorf("read script %s: %w", path, err)
				}
				b.ScriptContent = content
				return nil
			default:
				content, err := os.ReadFile(path) //nolint:gosec // path walks the operator-controlled skills root, never request input
				if err != nil {
					return fmt.Errorf("read %s: %w", path, err)
				}
				b.Files = append(b.Files, BundleFile{Path: rel, Content: content})
				return nil
			}
		}); err != nil {
			return nil, fmt.Errorf("skills: walk bundle %s: %w", dir, err)
		}

		bundles = append(bundles, b)
	}
	return bundles, nil
}
