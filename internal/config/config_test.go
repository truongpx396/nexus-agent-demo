package config

import (
	"testing"

	"github.com/google/uuid"
)

func TestDefaultFor_Unconfigured(t *testing.T) {
	tenantID := uuid.New()
	c := defaultFor(tenantID)
	if c.TenantID != tenantID {
		t.Errorf("defaultFor(%s).TenantID = %s, want %s", tenantID, c.TenantID, tenantID)
	}
	if c.MemoryRetentionDays != DefaultMemoryRetentionDays {
		t.Errorf("defaultFor(%s).MemoryRetentionDays = %d, want %d", tenantID, c.MemoryRetentionDays, DefaultMemoryRetentionDays)
	}
	if len(c.AdmittedSkillIDs) != 0 {
		t.Errorf("defaultFor(%s).AdmittedSkillIDs = %v, want empty", tenantID, c.AdmittedSkillIDs)
	}
	if len(c.AdmittedConnectorProviders) != 0 {
		t.Errorf("defaultFor(%s).AdmittedConnectorProviders = %v, want empty", tenantID, c.AdmittedConnectorProviders)
	}
}

func TestTenantConfig_HasConnectorProvider(t *testing.T) {
	c := TenantConfig{AdmittedConnectorProviders: []string{"google", "github"}}
	cases := []struct {
		provider string
		want     bool
	}{
		{"google", true},
		{"github", true},
		{"slack", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := c.HasConnectorProvider(tc.provider); got != tc.want {
			t.Errorf("HasConnectorProvider(%q) = %v, want %v", tc.provider, got, tc.want)
		}
	}
}

func TestTenantConfig_HasSkill(t *testing.T) {
	c := TenantConfig{AdmittedSkillIDs: []string{"triage-report", "send-email"}}
	cases := []struct {
		skillID string
		want    bool
	}{
		{"triage-report", true},
		{"send-email", true},
		{"delete-everything", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := c.HasSkill(tc.skillID); got != tc.want {
			t.Errorf("HasSkill(%q) = %v, want %v", tc.skillID, got, tc.want)
		}
	}
}
