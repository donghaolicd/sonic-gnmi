package common_utils

import "testing"

func TestGNMIAuditLostCounter(t *testing.T) {
	if got := GNMI_AUDIT_LOST.String(); got != "GNMI audit lost" {
		t.Fatalf("GNMI_AUDIT_LOST.String() = %q", got)
	}

	InitCounters()
	IncCounter(GNMI_AUDIT_LOST)
	var counters [COUNTER_SIZE]uint64
	if err := GetMemCounters(&counters); err != nil {
		t.Fatalf("GetMemCounters() failed: %v", err)
	}
	if got := counters[GNMI_AUDIT_LOST]; got != 1 {
		t.Fatalf("GNMI_AUDIT_LOST counter = %d, want 1", got)
	}
}
