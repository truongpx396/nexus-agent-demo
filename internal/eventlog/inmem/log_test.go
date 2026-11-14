package inmem_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/eventlog/inmem"
	"github.com/truongpx396/nexus-agent/internal/eventlog/logtest"
)

func TestLog_SatisfiesTheEventLogContract(t *testing.T) {
	log := inmem.New()
	tenantID := uuid.New()
	logtest.Run(t, logtest.Subject{
		Log:        log,
		NewSession: func(*testing.T) (uuid.UUID, uuid.UUID) { return tenantID, uuid.New() },
		Events: func(_ *testing.T, _, sessionID uuid.UUID) []eventlog.Event {
			return log.Events(sessionID)
		},
		Status: func(t *testing.T, _, sessionID uuid.UUID) (string, *string) {
			s, ok := log.Status(sessionID)
			if !ok {
				t.Fatalf("no status recorded for %s", sessionID)
			}
			return s.Value, s.TerminalReason
		},
		SetAutonomy: func(_ *testing.T, _, sessionID uuid.UUID, level string) { log.SetAutonomy(sessionID, level) },
	})
}
