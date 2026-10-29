package cost

import (
	"github.com/google/uuid"
)

// CostRecord is one meter's actual, reconciled cost for one model call —
// the durable truth Reconcile writes AFTER the call, as opposed to
// Decision's pre-spend estimate. Multiple CostRecords (one per meter with
// nonzero quantity) back a single Reconcile call, all inserted together
// (RecordUsage below).
type CostRecord struct {
	ID            uuid.UUID
	ReservationID *uuid.UUID // the Reservation this reconciles, nil for a direct Record call
	Meter         MeterID
	Quantity      int64
	Unit          string
	Cost          Money
	ModelID       string
	Unreported    bool // true when usage was UNREPORTED and this is the reserved worst-case, not measured usage (task 4.7)
}
