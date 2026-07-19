package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestReserveRuntimeStartsAbsentWithImmutableOwnership(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	runtime, err := domain.ReserveRuntime(domain.RuntimeReservation{
		ID: "runtime-1", EnvironmentID: "environment-1", Sequence: 1,
		RuntimePreset: "standard", Region: "us-east-1", AvailabilityZone: "us-east-1a",
		ImageVersion: "ubuntu-2026-07-13", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("ReserveRuntime(): %v", err)
	}
	snapshot := runtime.Snapshot()
	if snapshot.ID != "runtime-1" || snapshot.EnvironmentID != "environment-1" || snapshot.Sequence != 1 {
		t.Fatalf("Runtime identity = %#v", snapshot)
	}
	if snapshot.RuntimePreset != "standard" || snapshot.Region != "us-east-1" || snapshot.AvailabilityZone != "us-east-1a" || snapshot.ImageVersion != "ubuntu-2026-07-13" {
		t.Fatalf("Runtime ownership = %#v", snapshot)
	}
	if snapshot.Status != domain.RuntimeAbsent || snapshot.Version != 1 || !snapshot.CreatedAt.Equal(createdAt) || !snapshot.UpdatedAt.Equal(createdAt) {
		t.Fatalf("initial Runtime state = %#v", snapshot)
	}
	if snapshot.ProviderInstanceRef != nil || snapshot.PrivateAddress != nil || snapshot.BootID != nil || snapshot.StartedAt != nil || snapshot.StoppedAt != nil || snapshot.RetiredAt != nil {
		t.Fatalf("absent Runtime has observations = %#v", snapshot)
	}
}

func TestRuntimeMarksProvisionErrorWithoutFabricatingProviderIdentity(t *testing.T) {
	createdAt := time.Date(2026, time.July, 19, 10, 0, 0, 0, time.UTC)
	runtime := reservedRuntime(t, createdAt)
	failed, err := runtime.MarkProvisionError(createdAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("MarkProvisionError(): %v", err)
	}
	snapshot := failed.Snapshot()
	if snapshot.Status != domain.RuntimeError || snapshot.ProviderInstanceRef != nil || snapshot.Version != 2 {
		t.Fatalf("failed Runtime = %#v", snapshot)
	}
	if _, err := domain.RestoreRuntime(snapshot); err != nil {
		t.Fatalf("RestoreRuntime(failed): %v", err)
	}
}

func TestRuntimeProvisionAndStartAdvanceMonotonically(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	runtime := reservedRuntime(t, createdAt)
	provisionedAt := createdAt.Add(time.Minute)
	provisioning, err := runtime.Provision("i-runtime-1", provisionedAt)
	if err != nil {
		t.Fatalf("Provision(): %v", err)
	}
	provisioningSnapshot := provisioning.Snapshot()
	if provisioningSnapshot.Status != domain.RuntimeProvisioning || provisioningSnapshot.ProviderInstanceRef == nil || *provisioningSnapshot.ProviderInstanceRef != "i-runtime-1" || provisioningSnapshot.Version != 2 {
		t.Fatalf("provisioning Runtime = %#v", provisioningSnapshot)
	}

	startedAt := provisionedAt.Add(time.Minute)
	starting, err := provisioning.BeginStart(startedAt)
	if err != nil {
		t.Fatalf("BeginStart(): %v", err)
	}
	startingSnapshot := starting.Snapshot()
	if startingSnapshot.Status != domain.RuntimeStarting || startingSnapshot.StartedAt == nil || !startingSnapshot.StartedAt.Equal(startedAt) || startingSnapshot.Version != 3 || !startingSnapshot.UpdatedAt.Equal(startedAt) {
		t.Fatalf("starting Runtime = %#v", startingSnapshot)
	}
	if startingSnapshot.PrivateAddress != nil || startingSnapshot.BootID != nil || startingSnapshot.StoppedAt != nil {
		t.Fatalf("starting Runtime retained stale observations = %#v", startingSnapshot)
	}
	if _, err := starting.BeginStart(provisionedAt); err == nil {
		t.Fatal("BeginStart() accepted a stale transition")
	}
	if runtime.Snapshot().Status != domain.RuntimeAbsent || runtime.Snapshot().Version != 1 {
		t.Fatalf("original Runtime mutated = %#v", runtime.Snapshot())
	}
}

func TestRuntimeReadinessRequiresCurrentStartAndPrivateRoute(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	runtime := startingRuntime(t, createdAt)
	observedAt := createdAt.Add(3 * time.Minute)
	observation := domain.RuntimeReadinessObservation{
		ProviderInstanceRef: "i-runtime-1", BootID: "boot-current", PrivateAddress: "10.0.0.4",
		ExpectedVersion: runtime.Snapshot().Version, ObservedAt: observedAt,
	}
	stale := observation
	stale.ExpectedVersion--
	stale.BootID = "boot-stale"
	stale.PrivateAddress = "10.0.0.3"
	if _, err := runtime.MarkReady(stale); !errors.Is(err, domain.ErrStaleRuntimeObservation) {
		t.Fatalf("stale readiness error = %v", err)
	}
	public := observation
	public.PrivateAddress = "203.0.113.10"
	if _, err := runtime.MarkReady(public); err == nil {
		t.Fatal("MarkReady() accepted a public address")
	}

	ready, err := runtime.MarkReady(observation)
	if err != nil {
		t.Fatalf("MarkReady(): %v", err)
	}
	snapshot := ready.Snapshot()
	if snapshot.Status != domain.RuntimeReady || snapshot.BootID == nil || *snapshot.BootID != "boot-current" || snapshot.PrivateAddress == nil || *snapshot.PrivateAddress != "10.0.0.4" || snapshot.Version != observation.ExpectedVersion+1 {
		t.Fatalf("ready Runtime = %#v", snapshot)
	}
	replayed, err := ready.MarkReady(observation)
	if err != nil {
		t.Fatalf("MarkReady() replay: %v", err)
	}
	if replayed.Snapshot().Version != ready.Snapshot().Version {
		t.Fatalf("readiness replay advanced version: %#v", replayed.Snapshot())
	}
}

func TestRuntimeStopPreservesIdentityAndNextStartRejectsPriorBoot(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	ready, readiness := readyRuntime(t, createdAt)
	stoppingAt := createdAt.Add(4 * time.Minute)
	stopping, err := ready.BeginStop(stoppingAt)
	if err != nil {
		t.Fatalf("BeginStop(): %v", err)
	}
	if snapshot := stopping.Snapshot(); snapshot.Status != domain.RuntimeStopping || snapshot.PrivateAddress != nil || snapshot.BootID != nil {
		t.Fatalf("stopping Runtime = %#v", snapshot)
	}
	stoppedAt := stoppingAt.Add(time.Minute)
	stopped, err := stopping.MarkStopped(domain.RuntimeStateObservation{
		ProviderInstanceRef: "i-runtime-1", ExpectedVersion: stopping.Snapshot().Version, ObservedAt: stoppedAt,
	})
	if err != nil {
		t.Fatalf("MarkStopped(): %v", err)
	}
	stoppedSnapshot := stopped.Snapshot()
	if stoppedSnapshot.Status != domain.RuntimeStopped || stoppedSnapshot.StoppedAt == nil || !stoppedSnapshot.StoppedAt.Equal(stoppedAt) || stoppedSnapshot.ProviderInstanceRef == nil || *stoppedSnapshot.ProviderInstanceRef != "i-runtime-1" {
		t.Fatalf("stopped Runtime = %#v", stoppedSnapshot)
	}
	startingAgain, err := stopped.BeginStart(stoppedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("BeginStart() after stop: %v", err)
	}
	if _, err := startingAgain.MarkReady(readiness); !errors.Is(err, domain.ErrStaleRuntimeObservation) {
		t.Fatalf("prior-boot readiness error = %v", err)
	}
}

func TestRuntimeFailureRevokesWritableReadinessBeforeReplacement(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	ready, _ := readyRuntime(t, createdAt)
	observation := domain.RuntimeStateObservation{
		ProviderInstanceRef: "i-runtime-1", ExpectedVersion: ready.Snapshot().Version,
		ObservedAt: createdAt.Add(4 * time.Minute),
	}
	stale := observation
	stale.ExpectedVersion--
	if _, err := ready.MarkError(stale); !errors.Is(err, domain.ErrStaleRuntimeObservation) {
		t.Fatalf("stale failure error = %v", err)
	}
	failed, err := ready.MarkError(observation)
	if err != nil {
		t.Fatalf("MarkError(): %v", err)
	}
	if snapshot := failed.Snapshot(); snapshot.Status != domain.RuntimeError || snapshot.PrivateAddress != nil || snapshot.BootID != nil {
		t.Fatalf("failed Runtime = %#v", snapshot)
	}
	if _, err := failed.BeginReplacement(createdAt.Add(5 * time.Minute)); err != nil {
		t.Fatalf("BeginReplacement() after error: %v", err)
	}
}

func TestRestoreRuntimeRejectsInconsistentPersistedState(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	ready, _ := readyRuntime(t, createdAt)
	tests := []struct {
		name       string
		invalidate func(*domain.RuntimeSnapshot)
		want       string
	}{
		{name: "padded Runtime ID", invalidate: func(snapshot *domain.RuntimeSnapshot) { snapshot.ID = " runtime-1" }, want: "ID"},
		{name: "padded region", invalidate: func(snapshot *domain.RuntimeSnapshot) { snapshot.Region = "us-east-1 " }, want: "region"},
		{name: "padded Runtime Preset", invalidate: func(snapshot *domain.RuntimeSnapshot) { snapshot.RuntimePreset = " standard" }, want: "Runtime Preset"},
		{name: "padded provider", invalidate: func(snapshot *domain.RuntimeSnapshot) { value := " i-runtime-1"; snapshot.ProviderInstanceRef = &value }, want: "provider instance"},
		{name: "padded boot", invalidate: func(snapshot *domain.RuntimeSnapshot) { value := "boot-current "; snapshot.BootID = &value }, want: "boot ID"},
		{name: "missing boot", invalidate: func(snapshot *domain.RuntimeSnapshot) { snapshot.BootID = nil }, want: "boot ID"},
		{name: "public route", invalidate: func(snapshot *domain.RuntimeSnapshot) { address := "203.0.113.10"; snapshot.PrivateAddress = &address }, want: "private IPv4"},
		{name: "missing provider", invalidate: func(snapshot *domain.RuntimeSnapshot) { snapshot.ProviderInstanceRef = nil }, want: "provider instance"},
		{name: "started before creation", invalidate: func(snapshot *domain.RuntimeSnapshot) {
			startedAt := snapshot.CreatedAt.Add(-time.Second)
			snapshot.StartedAt = &startedAt
		}, want: "started time"},
		{name: "future observation", invalidate: func(snapshot *domain.RuntimeSnapshot) {
			startedAt := snapshot.UpdatedAt.Add(time.Second)
			snapshot.StartedAt = &startedAt
		}, want: "started time"},
		{name: "zero update", invalidate: func(snapshot *domain.RuntimeSnapshot) { snapshot.UpdatedAt = time.Time{} }, want: "update time"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := ready.Snapshot()
			test.invalidate(&snapshot)
			if _, err := domain.RestoreRuntime(snapshot); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RestoreRuntime() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestEnvironmentReplacementRequiresRetiredCurrentRuntime(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	old, _ := readyRuntime(t, createdAt)
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-1", OwnerUserID: "user-1", Name: "dev", Slug: "dev", Region: "us-east-1",
		AvailabilityZone: "us-east-1a", RuntimePreset: "standard", PinnedProfileVersionID: "profile-1",
		AutoStopPolicyID: "policy-1", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("ReserveEnvironment(): %v", err)
	}
	environment, err = environment.AttachRuntime(reservedRuntime(t, createdAt), createdAt.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("AttachRuntime(): %v", err)
	}
	replacement := reserveReplacementRuntime(t, old.Snapshot(), createdAt.Add(5*time.Minute))
	if _, err := environment.ReplaceRuntime(old, replacement, createdAt.Add(6*time.Minute)); err == nil {
		t.Fatal("ReplaceRuntime() accepted a writable old Runtime")
	}

	replacing, err := old.BeginReplacement(createdAt.Add(5 * time.Minute))
	if err != nil {
		t.Fatalf("BeginReplacement(): %v", err)
	}
	retired, err := replacing.Retire(domain.RuntimeStateObservation{
		ProviderInstanceRef: "i-runtime-1", ExpectedVersion: replacing.Snapshot().Version,
		ObservedAt: createdAt.Add(6 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Retire(): %v", err)
	}
	replaced, err := environment.ReplaceRuntime(retired, replacement, createdAt.Add(7*time.Minute))
	if err != nil {
		t.Fatalf("ReplaceRuntime(): %v", err)
	}
	snapshot := replaced.Snapshot()
	if snapshot.CurrentRuntimeID == nil || *snapshot.CurrentRuntimeID != replacement.Snapshot().ID {
		t.Fatalf("current Runtime = %v, want %q", snapshot.CurrentRuntimeID, replacement.Snapshot().ID)
	}
	if retired.Snapshot().RetiredAt == nil || retired.Snapshot().Status != domain.RuntimeAbsent {
		t.Fatalf("retired Runtime = %#v", retired.Snapshot())
	}
}

func TestEnvironmentStopPreservesCurrentRuntime(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	ready, _ := readyRuntime(t, createdAt)
	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID: "environment-1", OwnerUserID: "user-1", Name: "dev", Slug: "dev", Region: "us-east-1",
		AvailabilityZone: "us-east-1a", RuntimePreset: "standard", PinnedProfileVersionID: "profile-1",
		AutoStopPolicyID: "policy-1", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("ReserveEnvironment(): %v", err)
	}
	environment, err = environment.AttachRuntime(reservedRuntime(t, createdAt), createdAt.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("AttachRuntime(): %v", err)
	}
	stopping, err := ready.BeginStop(createdAt.Add(5 * time.Minute))
	if err != nil {
		t.Fatalf("BeginStop(): %v", err)
	}
	stopped, err := stopping.MarkStopped(domain.RuntimeStateObservation{
		ProviderInstanceRef: "i-runtime-1", ExpectedVersion: stopping.Snapshot().Version,
		ObservedAt: createdAt.Add(6 * time.Minute),
	})
	if err != nil {
		t.Fatalf("MarkStopped(): %v", err)
	}
	if stopped.Snapshot().Status != domain.RuntimeStopped || environment.Snapshot().CurrentRuntimeID == nil || *environment.Snapshot().CurrentRuntimeID != stopped.Snapshot().ID {
		t.Fatalf("stop changed ownership: Environment=%#v Runtime=%#v", environment.Snapshot(), stopped.Snapshot())
	}
}

func reservedRuntime(t *testing.T, createdAt time.Time) domain.Runtime {
	t.Helper()
	runtime, err := domain.ReserveRuntime(domain.RuntimeReservation{
		ID: "runtime-1", EnvironmentID: "environment-1", Sequence: 1,
		RuntimePreset: "standard", Region: "us-east-1", AvailabilityZone: "us-east-1a",
		ImageVersion: "ubuntu-2026-07-13", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("reserve Runtime fixture: %v", err)
	}
	return runtime
}

func startingRuntime(t *testing.T, createdAt time.Time) domain.Runtime {
	t.Helper()
	runtime, err := reservedRuntime(t, createdAt).Provision("i-runtime-1", createdAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("provision Runtime fixture: %v", err)
	}
	runtime, err = runtime.BeginStart(createdAt.Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("start Runtime fixture: %v", err)
	}
	return runtime
}

func readyRuntime(t *testing.T, createdAt time.Time) (domain.Runtime, domain.RuntimeReadinessObservation) {
	t.Helper()
	runtime := startingRuntime(t, createdAt)
	observation := domain.RuntimeReadinessObservation{
		ProviderInstanceRef: "i-runtime-1", BootID: "boot-1", PrivateAddress: "10.0.0.4",
		ExpectedVersion: runtime.Snapshot().Version, ObservedAt: createdAt.Add(3 * time.Minute),
	}
	ready, err := runtime.MarkReady(observation)
	if err != nil {
		t.Fatalf("ready Runtime fixture: %v", err)
	}
	return ready, observation
}

func reserveReplacementRuntime(t *testing.T, current domain.RuntimeSnapshot, createdAt time.Time) domain.Runtime {
	t.Helper()
	runtime, err := domain.ReserveRuntime(domain.RuntimeReservation{
		ID: "runtime-2", EnvironmentID: current.EnvironmentID, Sequence: current.Sequence + 1,
		RuntimePreset: current.RuntimePreset, Region: current.Region, AvailabilityZone: current.AvailabilityZone,
		ImageVersion: "ubuntu-next", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("reserve replacement Runtime: %v", err)
	}
	return runtime
}
