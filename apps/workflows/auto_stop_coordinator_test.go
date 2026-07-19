package workflows_test

import (
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestAutoStopCoordinatorStartsContinuesAndCancelsGraceByGeneration(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	policy := mustAutoStopPolicy(t, domain.AutoStopWhenDisconnected, 300)
	coordinator := workflows.AutoStopCoordinator{}
	state := workflows.AutoStopCoordinationState{EnvironmentID: "environment-1"}

	first := observe(t, coordinator, state, observation(policy, 1, activity(now, 1)))
	if first.Timer == nil || first.Timer.Generation == 0 || first.Timer.Delay != 5*time.Minute || !first.State.TimerPending {
		t.Fatalf("first transition = %#v", first)
	}
	continued := observe(t, coordinator, first.State, observation(policy, 1, activity(now.Add(time.Second), 2)))
	if continued.Timer != nil || continued.State.TimerGeneration != first.State.TimerGeneration || continued.State.LastSnapshotSequence != 2 {
		t.Fatalf("continued transition = %#v", continued)
	}
	active := activity(now.Add(2*time.Second), 3)
	active.SSHConnections = 1
	cancelled := observe(t, coordinator, continued.State, observation(policy, 1, active))
	if !cancelled.Cancelled || cancelled.State.TimerPending || cancelled.State.TimerGeneration <= continued.State.TimerGeneration {
		t.Fatalf("cancelled transition = %#v", cancelled)
	}
	staleExpiry := expire(t, coordinator, cancelled.State, workflows.AutoStopExpiry{
		RuntimeID: "runtime-1", Generation: first.Timer.Generation,
		Observation: observation(policy, 1, activity(now.Add(3*time.Second), 4)),
	})
	if staleExpiry.Stop != nil {
		t.Fatalf("cancelled generation dispatched stop: %#v", staleExpiry)
	}
}

func TestAutoStopCoordinatorResetsTimerOnPolicyAndRuntimeChanges(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	coordinator := workflows.AutoStopCoordinator{}
	state := workflows.AutoStopCoordinationState{EnvironmentID: "environment-1"}
	disconnected := mustAutoStopPolicy(t, domain.AutoStopWhenDisconnected, 60)
	started := observe(t, coordinator, state, observation(disconnected, 1, activity(now, 1)))

	agents := mustAutoStopPolicy(t, domain.AutoStopWhenAgentsFinish, 120)
	changed := observe(t, coordinator, started.State, observation(agents, 2, activity(now.Add(time.Second), 1)))
	if !changed.Cancelled || changed.Timer == nil || changed.Timer.Delay != 2*time.Minute || changed.Timer.Generation <= started.Timer.Generation {
		t.Fatalf("policy change transition = %#v", changed)
	}
	manual := mustAutoStopPolicy(t, domain.AutoStopManual, 0)
	disabled := observe(t, coordinator, changed.State, observation(manual, 3, nil))
	if !disabled.Cancelled || disabled.Timer != nil || disabled.State.TimerPending {
		t.Fatalf("manual transition = %#v", disabled)
	}

	runtimeChanged := observation(disconnected, 3, activity(now.Add(2*time.Second), 1))
	runtimeChanged.RuntimeID = "runtime-2"
	runtimeChanged.Snapshot.RuntimeID = "runtime-2"
	restarted := observe(t, coordinator, disabled.State, runtimeChanged)
	if restarted.Timer == nil || restarted.State.RuntimeID != "runtime-2" || restarted.State.LastSnapshotSequence != 1 {
		t.Fatalf("Runtime change transition = %#v", restarted)
	}
}

func TestAutoStopCoordinatorExpiryRequiresFreshCurrentSnapshotAndDispatchesOnce(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	policy := mustAutoStopPolicy(t, domain.AutoStopWhenFullyIdle, 10)
	coordinator := workflows.AutoStopCoordinator{}
	started := observe(t, coordinator, workflows.AutoStopCoordinationState{EnvironmentID: "environment-1"}, observation(policy, 7, activity(now, 10)))
	generation := started.Timer.Generation

	stale := expire(t, coordinator, started.State, workflows.AutoStopExpiry{
		RuntimeID: "runtime-1", Generation: generation,
		Observation: observation(policy, 7, activity(now.Add(time.Second), 10)),
	})
	if stale.Stop != nil || stale.State.TimerPending {
		t.Fatalf("stale snapshot expiry = %#v", stale)
	}

	restarted := observe(t, coordinator, stale.State, observation(policy, 7, activity(now.Add(2*time.Second), 11)))
	activeSnapshot := activity(now.Add(3*time.Second), 12)
	activeSnapshot.UnknownUserProcesses = 1
	active := expire(t, coordinator, restarted.State, workflows.AutoStopExpiry{
		RuntimeID: "runtime-1", Generation: restarted.Timer.Generation,
		Observation: observation(policy, 7, activeSnapshot),
	})
	if active.Stop != nil || active.State.TimerPending || active.Decision.Reason != domain.AutoStopBlockedActivity {
		t.Fatalf("activity at timer expiry = %#v", active)
	}

	restartObservation := observation(policy, 7, activity(now.Add(4*time.Second), 13))
	restartObservation.ProcessedAt = now.Add(5 * time.Second)
	restarted = observe(t, coordinator, active.State, restartObservation)
	freshGeneration := restarted.Timer.Generation
	expired := expire(t, coordinator, restarted.State, workflows.AutoStopExpiry{
		RuntimeID: "runtime-1", Generation: freshGeneration,
		Observation: observation(policy, 7, activity(now.Add(5*time.Second), 14)),
	})
	if expired.Stop == nil || expired.Stop.Reason != domain.RuntimeStopAutoStop || expired.Stop.RuntimeID != "runtime-1" || expired.Stop.IdempotencyKey == "" {
		t.Fatalf("fresh expiry = %#v", expired)
	}
	if evidence := expired.Stop.AuditEvidence; evidence == nil || evidence.Policy.ID != "policy-1" || evidence.PolicyGeneration != 7 ||
		len(evidence.QualifyingSnapshots) != 2 || evidence.QualifyingSnapshots[0].Sequence != 13 || evidence.QualifyingSnapshots[1].Sequence != 14 ||
		evidence.GracePeriodSeconds != 10 || !evidence.GraceStartedAt.Equal(restartObservation.ProcessedAt) || evidence.GraceExpiredAt.IsZero() {
		t.Fatalf("Auto-stop audit evidence = %#v", evidence)
	}
	replayed := expire(t, coordinator, expired.State, workflows.AutoStopExpiry{
		RuntimeID: "runtime-1", Generation: freshGeneration,
		Observation: observation(policy, 7, activity(now.Add(6*time.Second), 15)),
	})
	if replayed.Stop != nil {
		t.Fatalf("replayed expiry dispatched twice: %#v", replayed)
	}
}

func TestAutoStopCoordinatorSuppressesConflictAndRejectsStaleRuntimeExpiry(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	policy := mustAutoStopPolicy(t, domain.AutoStopWhenDisconnected, 10)
	coordinator := workflows.AutoStopCoordinator{}
	started := observe(t, coordinator, workflows.AutoStopCoordinationState{EnvironmentID: "environment-1"}, observation(policy, 1, activity(now, 1)))
	conflicted := observation(policy, 1, activity(now.Add(time.Second), 2))
	conflicted.Conflicts = []domain.AutoStopConflict{domain.AutoStopConflictReplace}
	blocked := observe(t, coordinator, started.State, conflicted)
	if !blocked.Cancelled || blocked.Timer != nil {
		t.Fatalf("conflicting operation did not cancel: %#v", blocked)
	}
	wrongRuntime := expire(t, coordinator, started.State, workflows.AutoStopExpiry{
		RuntimeID: "runtime-old", Generation: started.Timer.Generation,
		Observation: observation(policy, 1, activity(now.Add(2*time.Second), 2)),
	})
	if wrongRuntime.Stop != nil {
		t.Fatalf("stale Runtime expiry dispatched: %#v", wrongRuntime)
	}
}

func TestAutoStopCoordinatorSuppressesUntilTheRuntimeResumes(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	policy := mustAutoStopPolicy(t, domain.AutoStopWhenDisconnected, 60)
	coordinator := workflows.AutoStopCoordinator{}
	started := observe(t, coordinator, workflows.AutoStopCoordinationState{EnvironmentID: "environment-1"}, observation(policy, 1, activity(now, 1)))
	suppressed, err := coordinator.Suppress(started.State, "runtime-1")
	if err != nil {
		t.Fatalf("Suppress(): %v", err)
	}
	if !suppressed.Cancelled || suppressed.State.TimerPending || suppressed.State.SuppressedRuntimeID != "runtime-1" {
		t.Fatalf("suppressed transition = %#v", suppressed)
	}
	ignored := observe(t, coordinator, suppressed.State, observation(policy, 1, activity(now.Add(time.Second), 2)))
	if ignored.Timer != nil || ignored.State.TimerPending {
		t.Fatalf("suppressed observation scheduled timer: %#v", ignored)
	}
	resumed, err := coordinator.Resume(ignored.State, "runtime-1")
	if err != nil {
		t.Fatalf("Resume(): %v", err)
	}
	restarted := observe(t, coordinator, resumed.State, observation(policy, 1, activity(now.Add(2*time.Second), 3)))
	if restarted.Timer == nil || !restarted.State.TimerPending || restarted.State.SuppressedRuntimeID != "" {
		t.Fatalf("resumed transition = %#v", restarted)
	}
}

func TestAutoStopCoordinatorResumeStartsNewDispatchCycle(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	policy := mustAutoStopPolicy(t, domain.AutoStopWhenDisconnected, 60)
	tests := []struct {
		name                        string
		observationsWhileSuppressed int
	}{
		{name: "stop failed then idle again"},
		{name: "stop succeeded restart then idle again", observationsWhileSuppressed: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := workflows.AutoStopCoordinator{}
			started := observe(t, coordinator, workflows.AutoStopCoordinationState{EnvironmentID: "environment-1"}, observation(policy, 1, activity(now, 1)))
			first := expire(t, coordinator, started.State, workflows.AutoStopExpiry{
				RuntimeID: "runtime-1", Generation: started.Timer.Generation,
				Observation: observation(policy, 1, activity(now.Add(time.Minute), 2)),
			})
			if first.Stop == nil {
				t.Fatalf("first dispatch = %#v", first)
			}

			suppressed, err := coordinator.Suppress(first.State, "runtime-1")
			if err != nil {
				t.Fatalf("Suppress(): %v", err)
			}
			state := suppressed.State
			sequence := uint64(3)
			for range test.observationsWhileSuppressed {
				ignored := observe(t, coordinator, state, observation(policy, 1, activity(now.Add(time.Duration(sequence)*time.Minute), sequence)))
				if ignored.Timer != nil {
					t.Fatalf("suppressed observation scheduled timer: %#v", ignored)
				}
				state = ignored.State
				sequence++
			}
			resumed, err := coordinator.Resume(state, "runtime-1")
			if err != nil {
				t.Fatalf("Resume(): %v", err)
			}
			restarted := observe(t, coordinator, resumed.State, observation(policy, 1, activity(now.Add(time.Duration(sequence)*time.Minute), sequence)))
			if restarted.Timer == nil || restarted.Timer.Generation <= started.Timer.Generation {
				t.Fatalf("new dispatch cycle timer = %#v", restarted)
			}
			second := expire(t, coordinator, restarted.State, workflows.AutoStopExpiry{
				RuntimeID: "runtime-1", Generation: restarted.Timer.Generation,
				Observation: observation(policy, 1, activity(now.Add(time.Duration(sequence+1)*time.Minute), sequence+1)),
			})
			if second.Stop == nil || second.Stop.IdempotencyKey == first.Stop.IdempotencyKey {
				t.Fatalf("second dispatch = %#v; first = %#v", second, first)
			}
		})
	}
}

func observation(policy domain.AutoStopPolicy, generation uint64, snapshot *domain.AutoStopActivitySnapshot) workflows.AutoStopObservation {
	processedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	if snapshot != nil {
		processedAt = snapshot.ObservedAt
	}
	return workflows.AutoStopObservation{
		RuntimeID: "runtime-1", Policy: policy.Snapshot(), PolicyGeneration: generation,
		FreshAfter: time.Date(2026, time.July, 13, 11, 59, 0, 0, time.UTC), ProcessedAt: processedAt, Snapshot: snapshot,
	}
}

func activity(at time.Time, sequence uint64) *domain.AutoStopActivitySnapshot {
	return &domain.AutoStopActivitySnapshot{RuntimeID: "runtime-1", Sequence: sequence, ObservedAt: at}
}

func mustAutoStopPolicy(t *testing.T, mode domain.AutoStopMode, grace int) domain.AutoStopPolicy {
	t.Helper()
	policy, err := domain.NewAutoStopPolicy("policy-1", "environment-1", mode, grace)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func observe(t *testing.T, coordinator workflows.AutoStopCoordinator, state workflows.AutoStopCoordinationState, input workflows.AutoStopObservation) workflows.AutoStopTransition {
	t.Helper()
	transition, err := coordinator.Observe(state, input)
	if err != nil {
		t.Fatalf("Observe(): %v", err)
	}
	return transition
}

func expire(t *testing.T, coordinator workflows.AutoStopCoordinator, state workflows.AutoStopCoordinationState, input workflows.AutoStopExpiry) workflows.AutoStopTransition {
	t.Helper()
	transition, err := coordinator.Expire(state, input)
	if err != nil {
		t.Fatalf("Expire(): %v", err)
	}
	return transition
}
