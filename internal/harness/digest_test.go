package harness

import "testing"

func baseConfig() Config {
	return Config{
		SystemPromptVersion:   "v3",
		CatalogManifestDigest: []byte{0x01, 0x02},
		SkillSetDigest:        []byte{0x03, 0x04},
		SafetyPolicyVersion:   "2026-08-01",
		ApprovalPolicyVersion: "v1",
		PromptMode:            "full",
	}
}

func TestDigestIsDeterministic(t *testing.T) {
	c := baseConfig()
	d1 := Digest(c)
	d2 := Digest(c)
	if string(d1) != string(d2) {
		t.Fatal("Digest is not deterministic for an identical Config")
	}
}

// TestDigestIsSensitiveToEveryField is the test the plan promises: pinning
// behavior means a mid-run change to ANY one field must be detectable —
// harness_digest is only useful if it moves whenever behavior could have.
func TestDigestIsSensitiveToEveryField(t *testing.T) {
	base := Digest(baseConfig())

	mutations := map[string]func(*Config){
		"SystemPromptVersion":   func(c *Config) { c.SystemPromptVersion = "v4" },
		"CatalogManifestDigest": func(c *Config) { c.CatalogManifestDigest = []byte{0xFF} },
		"SkillSetDigest":        func(c *Config) { c.SkillSetDigest = []byte{0xFF} },
		"SafetyPolicyVersion":   func(c *Config) { c.SafetyPolicyVersion = "2026-09-01" },
		"ApprovalPolicyVersion": func(c *Config) { c.ApprovalPolicyVersion = "v2" },
		"PromptMode":            func(c *Config) { c.PromptMode = "minimal" },
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := baseConfig()
			mutate(&c)
			got := Digest(c)
			if string(got) == string(base) {
				t.Fatalf("changing %s did not change the digest — harness_digest would silently fail to pin behavior", name)
			}
		})
	}
}
