package domain_test

import (
	"testing"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestRuntimeStopReasonCanonicalValues(t *testing.T) {
	valid := []domain.RuntimeStopReason{
		domain.RuntimeStopManual, domain.RuntimeStopAutoStop, domain.RuntimeStopBilling, domain.RuntimeStopRepair, domain.RuntimeStopResize,
	}
	for _, reason := range valid {
		if !reason.Valid() {
			t.Fatalf("Runtime stop reason %q is invalid", reason)
		}
	}
	for _, reason := range []domain.RuntimeStopReason{"", "auto-stop", "billing_policy", "terminate"} {
		if reason.Valid() {
			t.Fatalf("non-canonical Runtime stop reason %q is valid", reason)
		}
	}
}
