package guest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
)

type mutableBootSource struct {
	identity guest.BootIdentity
	err      error
}

func (source *mutableBootSource) CurrentBoot(context.Context) (guest.BootIdentity, error) {
	return source.identity, source.err
}

func TestReadinessReporterAdvancesExactLevelsForCurrentBoot(t *testing.T) {
	allocatedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	identity := guest.BootIdentity{RuntimeID: "runtime-2", BootID: "boot-current", RuntimeSequence: 2}
	source := &mutableBootSource{identity: identity}
	reporter, allocated, err := guest.NewReadinessReporter(t.Context(), identity, source, allocatedAt)
	if err != nil {
		t.Fatalf("NewReadinessReporter(): %v", err)
	}
	if allocated.RuntimeID != identity.RuntimeID || allocated.BootID != identity.BootID || allocated.RuntimeSequence != identity.RuntimeSequence || allocated.Level != guest.ReadinessAllocated || !allocated.ObservedAt.Equal(allocatedAt) {
		t.Fatalf("allocated readiness = %#v", allocated)
	}
	levels := []guest.ReadinessLevel{
		guest.ReadinessDataMounted, guest.ReadinessSSHReady, guest.ReadinessProjectReady, guest.ReadinessAgentsValidated,
	}
	current := allocated
	for index, level := range levels {
		observedAt := allocatedAt.Add(time.Duration(index+1) * time.Minute)
		current, err = reporter.Advance(t.Context(), level, observedAt)
		if err != nil {
			t.Fatalf("Advance(%q): %v", level, err)
		}
		if current.Level != level || !current.ObservedAt.Equal(observedAt) {
			t.Fatalf("readiness at %q = %#v", level, current)
		}
	}
	snapshot, err := reporter.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot(): %v", err)
	}
	if snapshot != current {
		t.Fatalf("snapshot = %#v, want %#v", snapshot, current)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal readiness: %v", err)
	}
	for _, forbidden := range []string{"secret", "command", "environment", "payload"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("bounded readiness contains %q: %s", forbidden, encoded)
		}
	}
}

func TestCompareReadinessDefinesCanonicalOrdering(t *testing.T) {
	levels := []guest.ReadinessLevel{
		guest.ReadinessAllocated, guest.ReadinessDataMounted, guest.ReadinessSSHReady,
		guest.ReadinessProjectReady, guest.ReadinessAgentsValidated,
	}
	for index, level := range levels {
		if guest.CompareReadiness(level, level) != 0 {
			t.Fatalf("CompareReadiness(%q, itself) != 0", level)
		}
		if index > 0 && guest.CompareReadiness(level, levels[index-1]) <= 0 {
			t.Fatalf("CompareReadiness(%q, %q) did not advance", level, levels[index-1])
		}
	}
	if guest.CompareReadiness("unknown", guest.ReadinessAllocated) >= 0 {
		t.Fatal("unknown readiness did not sort before allocated")
	}
}

func TestReadinessReporterRejectsSkippedRegressedAndStaleTransitions(t *testing.T) {
	allocatedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	identity := guest.BootIdentity{RuntimeID: "runtime-1", BootID: "boot-1", RuntimeSequence: 1}
	source := &mutableBootSource{identity: identity}
	reporter, _, err := guest.NewReadinessReporter(t.Context(), identity, source, allocatedAt)
	if err != nil {
		t.Fatalf("NewReadinessReporter(): %v", err)
	}
	if _, err := reporter.Advance(t.Context(), guest.ReadinessSSHReady, allocatedAt.Add(time.Minute)); err == nil {
		t.Fatal("Advance() skipped data-mounted readiness")
	}
	mounted, err := reporter.Advance(t.Context(), guest.ReadinessDataMounted, allocatedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("Advance(data mounted): %v", err)
	}
	replayed, err := reporter.Advance(t.Context(), guest.ReadinessDataMounted, allocatedAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Advance() replay: %v", err)
	}
	if replayed != mounted {
		t.Fatalf("replayed readiness = %#v, want %#v", replayed, mounted)
	}
	if _, err := reporter.Advance(t.Context(), guest.ReadinessAllocated, allocatedAt.Add(2*time.Minute)); err == nil {
		t.Fatal("Advance() regressed readiness")
	}
	if _, err := reporter.Advance(t.Context(), guest.ReadinessSSHReady, allocatedAt.Add(-time.Second)); err == nil {
		t.Fatal("Advance() accepted stale timestamp")
	}
}

func TestReadinessReporterCannotEmitAfterBootOrRuntimeReplacement(t *testing.T) {
	allocatedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	identity := guest.BootIdentity{RuntimeID: "runtime-1", BootID: "boot-1", RuntimeSequence: 1}
	tests := []struct {
		name    string
		current guest.BootIdentity
	}{
		{name: "new boot", current: guest.BootIdentity{RuntimeID: "runtime-1", BootID: "boot-2", RuntimeSequence: 1}},
		{name: "replacement Runtime", current: guest.BootIdentity{RuntimeID: "runtime-2", BootID: "boot-2", RuntimeSequence: 2}},
		{name: "sequence drift", current: guest.BootIdentity{RuntimeID: "runtime-1", BootID: "boot-1", RuntimeSequence: 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &mutableBootSource{identity: identity}
			reporter, _, err := guest.NewReadinessReporter(t.Context(), identity, source, allocatedAt)
			if err != nil {
				t.Fatalf("NewReadinessReporter(): %v", err)
			}
			source.identity = test.current
			if _, err := reporter.Snapshot(t.Context()); !errors.Is(err, guest.ErrStaleBootReadiness) {
				t.Fatalf("Snapshot() stale error = %v", err)
			}
			if _, err := reporter.Advance(t.Context(), guest.ReadinessDataMounted, allocatedAt.Add(time.Minute)); !errors.Is(err, guest.ErrStaleBootReadiness) {
				t.Fatalf("Advance() stale error = %v", err)
			}
		})
	}
}

func TestNewReadinessReporterRejectsInvalidOrNoncurrentIdentity(t *testing.T) {
	at := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	valid := guest.BootIdentity{RuntimeID: "runtime-1", BootID: "boot-1", RuntimeSequence: 1}
	tests := []struct {
		name     string
		identity guest.BootIdentity
		source   *mutableBootSource
	}{
		{name: "missing Runtime", identity: guest.BootIdentity{BootID: "boot-1", RuntimeSequence: 1}, source: &mutableBootSource{identity: valid}},
		{name: "missing boot", identity: guest.BootIdentity{RuntimeID: "runtime-1", RuntimeSequence: 1}, source: &mutableBootSource{identity: valid}},
		{name: "zero sequence", identity: guest.BootIdentity{RuntimeID: "runtime-1", BootID: "boot-1"}, source: &mutableBootSource{identity: valid}},
		{name: "noncurrent", identity: valid, source: &mutableBootSource{identity: guest.BootIdentity{RuntimeID: "runtime-2", BootID: "boot-2", RuntimeSequence: 2}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := guest.NewReadinessReporter(t.Context(), test.identity, test.source, at); err == nil {
				t.Fatal("NewReadinessReporter() accepted invalid identity")
			}
		})
	}
}
