package domain_test

import (
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestNewAutoStopPolicyAcceptsSupportedModes(t *testing.T) {
	modes := []domain.AutoStopMode{
		domain.AutoStopWhenDisconnected,
		domain.AutoStopWhenAgentsFinish,
		domain.AutoStopWhenFullyIdle,
		domain.AutoStopManual,
	}
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			policy, err := domain.NewAutoStopPolicy("policy-1", "environment-1", mode, 300)
			if err != nil {
				t.Fatalf("NewAutoStopPolicy(): %v", err)
			}
			if got := policy.Snapshot(); got.Mode != mode || got.GracePeriodSeconds != 300 {
				t.Fatalf("snapshot = %#v", got)
			}
		})
	}
}

func TestAutoStopPolicyEvaluatesEveryPredicateWithoutCPUHeuristics(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	base := domain.AutoStopActivitySnapshot{RuntimeID: "runtime-1", Sequence: 2, ObservedAt: now}
	tests := []struct {
		name      string
		mode      domain.AutoStopMode
		mutate    func(*domain.AutoStopActivitySnapshot)
		qualifies bool
	}{
		{name: "disconnected", mode: domain.AutoStopWhenDisconnected, qualifies: true},
		{name: "SSH connected", mode: domain.AutoStopWhenDisconnected, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.SSHConnections = 1 }},
		{name: "IDE connected", mode: domain.AutoStopWhenDisconnected, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.IDEConnections = 1 }},
		{name: "agents finished despite connection", mode: domain.AutoStopWhenAgentsFinish, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.SSHConnections = 1 }, qualifies: true},
		{name: "Codex waiting is active", mode: domain.AutoStopWhenAgentsFinish, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.CodexProcesses = 1 }},
		{name: "Claude waiting is active", mode: domain.AutoStopWhenAgentsFinish, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.ClaudeProcesses = 1 }},
		{name: "fully idle", mode: domain.AutoStopWhenFullyIdle, qualifies: true},
		{name: "fully idle blocks SSH", mode: domain.AutoStopWhenFullyIdle, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.SSHConnections = 1 }},
		{name: "fully idle blocks IDE", mode: domain.AutoStopWhenFullyIdle, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.IDEConnections = 1 }},
		{name: "fully idle blocks Codex", mode: domain.AutoStopWhenFullyIdle, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.CodexProcesses = 1 }},
		{name: "fully idle blocks Claude", mode: domain.AutoStopWhenFullyIdle, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.ClaudeProcesses = 1 }},
		{name: "fully idle blocks unknown", mode: domain.AutoStopWhenFullyIdle, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.UnknownUserProcesses = 1 }},
		{name: "fully idle blocks protected", mode: domain.AutoStopWhenFullyIdle, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.ProtectedProcesses = 1 }},
		{name: "fully idle blocks container", mode: domain.AutoStopWhenFullyIdle, mutate: func(snapshot *domain.AutoStopActivitySnapshot) { snapshot.SelectedContainers = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, err := domain.NewAutoStopPolicy("policy-1", "environment-1", test.mode, 300)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := base
			if test.mutate != nil {
				test.mutate(&snapshot)
			}
			decision, err := policy.Evaluate(domain.AutoStopEvaluationRequest{
				RuntimeID: "runtime-1", PreviousSnapshotSequence: 1,
				FreshAfter: now.Add(-time.Minute), Snapshot: &snapshot,
			})
			if err != nil {
				t.Fatalf("Evaluate(): %v", err)
			}
			if decision.Qualifies != test.qualifies {
				t.Fatalf("decision = %#v", decision)
			}
		})
	}
}

func TestAutoStopPolicyBlocksMissingStaleNonMonotonicAndManualSnapshots(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	policy, _ := domain.NewAutoStopPolicy("policy-1", "environment-1", domain.AutoStopWhenDisconnected, 300)
	tests := []struct {
		name    string
		request domain.AutoStopEvaluationRequest
		reason  domain.AutoStopDecisionReason
	}{
		{name: "missing", request: domain.AutoStopEvaluationRequest{RuntimeID: "runtime-1", FreshAfter: now}, reason: domain.AutoStopBlockedMissingSnapshot},
		{name: "stale time", request: domain.AutoStopEvaluationRequest{RuntimeID: "runtime-1", FreshAfter: now, Snapshot: &domain.AutoStopActivitySnapshot{RuntimeID: "runtime-1", Sequence: 2, ObservedAt: now.Add(-time.Second)}}, reason: domain.AutoStopBlockedStaleSnapshot},
		{name: "stale sequence", request: domain.AutoStopEvaluationRequest{RuntimeID: "runtime-1", PreviousSnapshotSequence: 2, FreshAfter: now, Snapshot: &domain.AutoStopActivitySnapshot{RuntimeID: "runtime-1", Sequence: 2, ObservedAt: now}}, reason: domain.AutoStopBlockedStaleSequence},
		{name: "wrong Runtime", request: domain.AutoStopEvaluationRequest{RuntimeID: "runtime-1", FreshAfter: now, Snapshot: &domain.AutoStopActivitySnapshot{RuntimeID: "runtime-old", Sequence: 2, ObservedAt: now}}, reason: domain.AutoStopBlockedRuntimeChanged},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := policy.Evaluate(test.request)
			if err != nil || decision.Qualifies || decision.Reason != test.reason {
				t.Fatalf("decision=%#v err=%v", decision, err)
			}
		})
	}
	manual, _ := domain.NewAutoStopPolicy("policy-1", "environment-1", domain.AutoStopManual, 0)
	decision, err := manual.Evaluate(domain.AutoStopEvaluationRequest{})
	if err != nil || decision.Qualifies || decision.Reason != domain.AutoStopBlockedManual {
		t.Fatalf("manual decision=%#v err=%v", decision, err)
	}
}

func TestAutoStopPolicySuppressesEveryConflictingOperation(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	policy, _ := domain.NewAutoStopPolicy("policy-1", "environment-1", domain.AutoStopWhenFullyIdle, 300)
	for _, conflict := range []domain.AutoStopConflict{
		domain.AutoStopConflictSetup, domain.AutoStopConflictMaterialization, domain.AutoStopConflictStart,
		domain.AutoStopConflictReplace, domain.AutoStopConflictRestore,
	} {
		t.Run(string(conflict), func(t *testing.T) {
			decision, err := policy.Evaluate(domain.AutoStopEvaluationRequest{
				RuntimeID: "runtime-1", FreshAfter: now.Add(-time.Minute),
				Snapshot:  &domain.AutoStopActivitySnapshot{RuntimeID: "runtime-1", Sequence: 1, ObservedAt: now},
				Conflicts: []domain.AutoStopConflict{conflict},
			})
			if err != nil || decision.Qualifies || decision.Reason != domain.AutoStopBlockedConflict {
				t.Fatalf("decision=%#v err=%v", decision, err)
			}
		})
	}
	_, err := policy.Evaluate(domain.AutoStopEvaluationRequest{
		RuntimeID: "runtime-1", FreshAfter: now.Add(-time.Minute),
		Snapshot:  &domain.AutoStopActivitySnapshot{RuntimeID: "runtime-1", Sequence: 1, ObservedAt: now},
		Conflicts: []domain.AutoStopConflict{domain.AutoStopConflictSetup, "unknown"},
	})
	if err == nil {
		t.Fatal("Evaluate() accepted an unknown conflict after a recognized conflict")
	}
}

func TestNewAutoStopPolicyRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name        string
		id          string
		environment string
		mode        domain.AutoStopMode
		grace       int
	}{
		{name: "missing ID", environment: "environment-1", mode: domain.AutoStopManual},
		{name: "missing Environment", id: "policy-1", mode: domain.AutoStopManual},
		{name: "unknown mode", id: "policy-1", environment: "environment-1", mode: "eventually"},
		{name: "negative grace", id: "policy-1", environment: "environment-1", mode: domain.AutoStopManual, grace: -1},
		{name: "excessive grace", id: "policy-1", environment: "environment-1", mode: domain.AutoStopManual, grace: 86401},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := domain.NewAutoStopPolicy(test.id, test.environment, test.mode, test.grace); err == nil {
				t.Fatal("NewAutoStopPolicy() error = nil")
			}
		})
	}
}
