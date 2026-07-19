//go:build !race

// Restate SDK v1.0.0's test HTTP/2 server races in its request-body drain path.
// Keep the real-server tracer in normal tests; race-test the coordinator separately.
package workflows_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
)

func TestAutoStopObjectRefreshesAtExpiryAndDispatchesOneIdempotentStop(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	policy := mustAutoStopPolicy(t, domain.AutoStopWhenFullyIdle, 0)
	source := &refreshSource{observation: observation(policy, 1, activity(now.Add(time.Second), 2))}
	dispatcher := newStopDispatcher()
	environment := testfixtures.StartRestate(t, workflows.AutoStopDefinition(source, dispatcher))
	client := workflows.NewClient(environment.Ingress())
	input := observation(policy, 1, activity(now, 1))

	if err := client.SendAutoStopObservation(t.Context(), "environment-1", input, "observe-1"); err != nil {
		t.Fatalf("send Auto-stop observation: %v", err)
	}
	if err := client.SendAutoStopObservation(t.Context(), "environment-1", input, "observe-1"); err != nil {
		t.Fatalf("replay Auto-stop observation: %v", err)
	}
	select {
	case request := <-dispatcher.requests:
		evidence := request.AuditEvidence
		if request.EnvironmentID != "environment-1" || request.RuntimeID != "runtime-1" || request.Reason != domain.RuntimeStopAutoStop || request.IdempotencyKey == "" ||
			evidence == nil || len(evidence.QualifyingSnapshots) != 2 || !evidence.GraceStartedAt.After(input.Snapshot.ObservedAt) ||
			evidence.GraceExpiredAt.Before(evidence.GraceStartedAt) {
			t.Fatalf("Runtime stop request = %#v", request)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for automatic Runtime stop")
	}
	select {
	case duplicate := <-dispatcher.requests:
		t.Fatalf("duplicate Runtime stop = %#v", duplicate)
	case <-time.After(500 * time.Millisecond):
	}
	if calls, after, freshAfter := source.snapshot(); calls != 1 || after != 1 || freshAfter.IsZero() {
		t.Fatalf("fresh Activity Snapshot requests = %d, after sequence %d, freshness threshold %v", calls, after, freshAfter)
	}
}

func TestAutoStopObjectEndsConflictingDispatchCycle(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	policy := mustAutoStopPolicy(t, domain.AutoStopWhenFullyIdle, 0)
	source := &refreshSource{observation: observation(policy, 1, activity(now.Add(time.Second), 2))}
	dispatcher := &conflictingStopDispatcher{keys: make(chan string, 8)}
	environment := testfixtures.StartRestate(t, workflows.AutoStopDefinition(source, dispatcher))
	client := workflows.NewClient(environment.Ingress())

	if err := client.SendAutoStopObservation(t.Context(), "environment-1", observation(policy, 1, activity(now, 1)), "observe-conflict-1"); err != nil {
		t.Fatal(err)
	}
	firstKey := <-dispatcher.keys
	if len(firstKey) < len(domain.SystemIdempotencyKeyPrefix) || firstKey[:len(domain.SystemIdempotencyKeyPrefix)] != domain.SystemIdempotencyKeyPrefix {
		t.Fatalf("automatic stop key = %q, want reserved system prefix", firstKey)
	}
	if err := client.SendAutoStopObservation(t.Context(), "environment-1", observation(policy, 1, activity(now.Add(2*time.Second), 3)), "observe-conflict-2"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case key := <-dispatcher.keys:
			if key == firstKey {
				t.Fatalf("conflicting automatic stop retried the same cycle with key %q", key)
			}
			if len(key) < len(domain.SystemIdempotencyKeyPrefix) || key[:len(domain.SystemIdempotencyKeyPrefix)] != domain.SystemIdempotencyKeyPrefix {
				t.Fatalf("automatic stop key = %q, want reserved system prefix", key)
			}
			return
		case <-deadline:
			t.Fatal("conflicting dispatch did not clear timer state for a later cycle")
		}
	}
}

func TestAutoStopRefreshDoesNotLivelockAfterRuntimeReplacement(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	oldPolicy := mustAutoStopPolicy(t, domain.AutoStopWhenFullyIdle, 3600)
	newPolicy := mustAutoStopPolicy(t, domain.AutoStopWhenFullyIdle, 0)
	newObservation := observation(newPolicy, 2, activity(now.Add(time.Second), 1))
	newObservation.RuntimeID = "runtime-2"
	newObservation.Snapshot.RuntimeID = "runtime-2"
	source := replacedRuntimeSource{observation: newObservation}
	dispatcher := newStopDispatcher()
	environment := testfixtures.StartRestate(t, workflows.AutoStopDefinition(source, dispatcher))
	client := workflows.NewClient(environment.Ingress())

	if err := client.SendAutoStopObservation(t.Context(), "environment-1", observation(oldPolicy, 1, activity(now, 1)), "observe-old-runtime"); err != nil {
		t.Fatal(err)
	}
	if err := client.SendAutoStopPolicyRefresh(t.Context(), "environment-1", "refresh-after-replacement"); err != nil {
		t.Fatal(err)
	}
	if err := client.SendAutoStopObservation(t.Context(), "environment-1", newObservation, "observe-new-runtime"); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-dispatcher.requests:
		if request.RuntimeID != "runtime-2" {
			t.Fatalf("automatic stop Runtime = %q", request.RuntimeID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale Runtime refresh blocked the replacement Runtime observation")
	}
}

type refreshSource struct {
	mu          sync.Mutex
	observation workflows.AutoStopObservation
	calls       int
	after       uint64
	freshAfter  time.Time
}

func (source *refreshSource) RefreshAutoStop(_ context.Context, request workflows.AutoStopRefreshRequest) (workflows.AutoStopObservation, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	source.after = request.AfterSnapshotSequence
	source.freshAfter = request.FreshAfter
	observation := source.observation
	observation.FreshAfter = time.Time{}
	snapshot := *observation.Snapshot
	if snapshot.Sequence <= request.AfterSnapshotSequence {
		snapshot.Sequence = request.AfterSnapshotSequence + 1
	}
	snapshot.ObservedAt = request.FreshAfter.Add(time.Millisecond)
	observation.Snapshot = &snapshot
	return observation, nil
}

func (source *refreshSource) snapshot() (int, uint64, time.Time) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls, source.after, source.freshAfter
}

type stopDispatcher struct {
	mu       sync.Mutex
	seen     map[string]struct{}
	requests chan workflows.RuntimeStopRequest
}

func newStopDispatcher() *stopDispatcher {
	return &stopDispatcher{seen: make(map[string]struct{}), requests: make(chan workflows.RuntimeStopRequest, 2)}
}

func (dispatcher *stopDispatcher) DispatchRuntimeStop(_ context.Context, request workflows.RuntimeStopRequest) error {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if _, duplicate := dispatcher.seen[request.IdempotencyKey]; duplicate {
		return nil
	}
	dispatcher.seen[request.IdempotencyKey] = struct{}{}
	dispatcher.requests <- request
	return nil
}

type conflictingStopDispatcher struct{ keys chan string }

func (dispatcher *conflictingStopDispatcher) DispatchRuntimeStop(_ context.Context, request workflows.RuntimeStopRequest) error {
	dispatcher.keys <- request.IdempotencyKey
	return fmt.Errorf("reserve automatic stop: %w", db.ErrIdempotencyConflict)
}

type replacedRuntimeSource struct{ observation workflows.AutoStopObservation }

func (source replacedRuntimeSource) RefreshAutoStop(_ context.Context, request workflows.AutoStopRefreshRequest) (workflows.AutoStopObservation, error) {
	if request.RuntimeID == "runtime-1" {
		return workflows.AutoStopObservation{}, db.ErrReferenceNotOwned
	}
	observation := source.observation
	snapshot := *observation.Snapshot
	snapshot.Sequence = request.AfterSnapshotSequence + 1
	snapshot.ObservedAt = request.FreshAfter.Add(time.Millisecond)
	observation.Snapshot = &snapshot
	return observation, nil
}
