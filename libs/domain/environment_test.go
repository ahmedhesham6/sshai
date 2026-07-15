package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestReserveEnvironmentStartsWithoutRuntime(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)

	environment, err := domain.ReserveEnvironment(domain.EnvironmentReservation{
		ID:                     "env_01",
		OwnerUserID:            "usr_01",
		Name:                   "api-dev",
		Slug:                   "api-dev",
		Region:                 "us-east-1",
		AvailabilityZone:       "us-east-1a",
		RuntimePreset:          "standard",
		PinnedProfileVersionID: "prv_01",
		AutoStopPolicyID:       "asp_01",
		CreatedAt:              createdAt,
	})
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	snapshot := environment.Snapshot()

	if snapshot.Lifecycle != domain.EnvironmentCreating {
		t.Errorf("lifecycle = %q, want %q", snapshot.Lifecycle, domain.EnvironmentCreating)
	}
	if snapshot.Health != domain.EnvironmentHealthUnknown {
		t.Errorf("health = %q, want %q", snapshot.Health, domain.EnvironmentHealthUnknown)
	}
	if snapshot.CurrentRuntimeID != nil {
		t.Errorf("current Runtime = %q, want none", *snapshot.CurrentRuntimeID)
	}
	if snapshot.Version != 1 {
		t.Errorf("version = %d, want 1", snapshot.Version)
	}
	if !snapshot.CreatedAt.Equal(createdAt) || !snapshot.UpdatedAt.Equal(createdAt) {
		t.Errorf("timestamps = (%s, %s), want %s", snapshot.CreatedAt, snapshot.UpdatedAt, createdAt)
	}
}

func TestReserveEnvironmentRejectsInvalidReservation(t *testing.T) {
	_, err := domain.ReserveEnvironment(domain.EnvironmentReservation{})
	if err == nil || !strings.Contains(err.Error(), "ID") {
		t.Fatalf("reserve Environment error = %v, want containing %q", err, "ID")
	}
}

func TestRestoreEnvironmentRejectsInvalidSnapshot(t *testing.T) {
	tests := []struct {
		name       string
		invalidate func(*domain.EnvironmentSnapshot)
		wantError  string
	}{
		{name: "missing ID", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.ID = "" }, wantError: "ID"},
		{name: "missing owner", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.OwnerUserID = "" }, wantError: "owner User ID"},
		{name: "missing name", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.Name = "" }, wantError: "name"},
		{name: "missing slug", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.Slug = "" }, wantError: "slug"},
		{name: "unknown lifecycle", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.Lifecycle = "paused" }, wantError: "lifecycle"},
		{name: "unknown health", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.Health = "online" }, wantError: "health"},
		{name: "missing region", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.Region = "" }, wantError: "region"},
		{name: "padded region", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.Region = "us-east-1 " }, wantError: "region"},
		{name: "missing availability zone", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.AvailabilityZone = "" }, wantError: "availability zone"},
		{name: "padded availability zone", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.AvailabilityZone = " us-east-1a" }, wantError: "availability zone"},
		{name: "missing Runtime Preset", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.RuntimePreset = "" }, wantError: "Runtime Preset"},
		{name: "padded Runtime Preset", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.RuntimePreset = "standard " }, wantError: "Runtime Preset"},
		{name: "missing Profile Version", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.PinnedProfileVersionID = "" }, wantError: "Profile Version ID"},
		{name: "empty current Runtime ID", invalidate: func(snapshot *domain.EnvironmentSnapshot) { empty := ""; snapshot.CurrentRuntimeID = &empty }, wantError: "current Runtime ID"},
		{name: "padded current Runtime ID", invalidate: func(snapshot *domain.EnvironmentSnapshot) { value := " run_01"; snapshot.CurrentRuntimeID = &value }, wantError: "current Runtime ID"},
		{name: "missing Auto-stop Policy", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.AutoStopPolicyID = "" }, wantError: "Auto-stop Policy ID"},
		{name: "missing creation time", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.CreatedAt = time.Time{} }, wantError: "creation time"},
		{name: "missing update time", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.UpdatedAt = time.Time{} }, wantError: "update time"},
		{name: "update before creation", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.UpdatedAt = snapshot.CreatedAt.Add(-time.Second) }, wantError: "before creation"},
		{name: "deletion time before deletion", invalidate: func(snapshot *domain.EnvironmentSnapshot) {
			deletedAt := snapshot.CreatedAt
			snapshot.DeletedAt = &deletedAt
		}, wantError: "deletion time"},
		{name: "zero version", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.Version = 0 }, wantError: "version"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := validEnvironmentSnapshot()
			test.invalidate(&snapshot)

			_, err := domain.RestoreEnvironment(snapshot)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("restore Environment error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestRestoreEnvironmentRejectsInvalidDeletedState(t *testing.T) {
	tests := []struct {
		name       string
		invalidate func(*domain.EnvironmentSnapshot)
		wantError  string
	}{
		{name: "missing deletion time", invalidate: func(snapshot *domain.EnvironmentSnapshot) { snapshot.DeletedAt = nil }, wantError: "deletion time"},
		{name: "current Runtime remains", invalidate: func(snapshot *domain.EnvironmentSnapshot) {
			runtimeID := "run_01"
			snapshot.CurrentRuntimeID = &runtimeID
		}, wantError: "current Runtime"},
		{name: "deletion before creation", invalidate: func(snapshot *domain.EnvironmentSnapshot) {
			deletedAt := snapshot.CreatedAt.Add(-time.Second)
			snapshot.DeletedAt = &deletedAt
		}, wantError: "before creation"},
		{name: "deletion differs from update", invalidate: func(snapshot *domain.EnvironmentSnapshot) {
			deletedAt := snapshot.UpdatedAt.Add(time.Second)
			snapshot.DeletedAt = &deletedAt
		}, wantError: "update time"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := validEnvironmentSnapshot()
			snapshot.Lifecycle = domain.EnvironmentDeleted
			deletedAt := snapshot.UpdatedAt
			snapshot.DeletedAt = &deletedAt
			test.invalidate(&snapshot)

			_, err := domain.RestoreEnvironment(snapshot)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("restore deleted Environment error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestActivateEnvironmentReturnsNewActiveVersion(t *testing.T) {
	original, err := domain.ReserveEnvironment(validEnvironmentReservation())
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	activatedAt := validEnvironmentReservation().CreatedAt.Add(time.Minute)

	activated, err := original.Activate(activatedAt)
	if err != nil {
		t.Fatalf("activate Environment: %v", err)
	}

	originalSnapshot := original.Snapshot()
	if originalSnapshot.Lifecycle != domain.EnvironmentCreating || originalSnapshot.Version != 1 {
		t.Fatalf("original Environment changed: lifecycle = %q, version = %d", originalSnapshot.Lifecycle, originalSnapshot.Version)
	}
	activatedSnapshot := activated.Snapshot()
	if activatedSnapshot.Lifecycle != domain.EnvironmentActive {
		t.Errorf("activated lifecycle = %q, want %q", activatedSnapshot.Lifecycle, domain.EnvironmentActive)
	}
	if activatedSnapshot.Version != 2 {
		t.Errorf("activated version = %d, want 2", activatedSnapshot.Version)
	}
	if !activatedSnapshot.UpdatedAt.Equal(activatedAt) {
		t.Errorf("activated update time = %s, want %s", activatedSnapshot.UpdatedAt, activatedAt)
	}
}

func TestActivateEnvironmentRejectsInvalidTransition(t *testing.T) {
	original, err := domain.ReserveEnvironment(validEnvironmentReservation())
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	activated, err := original.Activate(validEnvironmentReservation().CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("activate Environment: %v", err)
	}

	if _, err := activated.Activate(validEnvironmentReservation().CreatedAt.Add(2 * time.Minute)); err == nil {
		t.Error("activate active Environment succeeded, want error")
	}
	if _, err := original.Activate(validEnvironmentReservation().CreatedAt.Add(-time.Second)); err == nil {
		t.Error("activate Environment in the past succeeded, want error")
	}
}

func TestBeginEnvironmentDeletionReturnsNewDeletingVersion(t *testing.T) {
	active := activeEnvironment(t)
	deletingAt := validEnvironmentReservation().CreatedAt.Add(2 * time.Minute)

	deleting, err := active.BeginDeletion(deletingAt)
	if err != nil {
		t.Fatalf("begin Environment deletion: %v", err)
	}

	if active.Snapshot().Lifecycle != domain.EnvironmentActive {
		t.Fatalf("original lifecycle = %q, want %q", active.Snapshot().Lifecycle, domain.EnvironmentActive)
	}
	snapshot := deleting.Snapshot()
	if snapshot.Lifecycle != domain.EnvironmentDeleting {
		t.Errorf("deleting lifecycle = %q, want %q", snapshot.Lifecycle, domain.EnvironmentDeleting)
	}
	if snapshot.Version != 3 {
		t.Errorf("deleting version = %d, want 3", snapshot.Version)
	}
	if !snapshot.UpdatedAt.Equal(deletingAt) {
		t.Errorf("deleting update time = %s, want %s", snapshot.UpdatedAt, deletingAt)
	}
}

func TestBeginEnvironmentDeletionRejectsInvalidTransition(t *testing.T) {
	creating, err := domain.ReserveEnvironment(validEnvironmentReservation())
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	if _, err := creating.BeginDeletion(validEnvironmentReservation().CreatedAt.Add(time.Minute)); err == nil {
		t.Error("begin deletion of creating Environment succeeded, want error")
	}

	active := activeEnvironment(t)
	if _, err := active.BeginDeletion(validEnvironmentReservation().CreatedAt); err == nil {
		t.Error("begin Environment deletion in the past succeeded, want error")
	}
}

func TestCompleteEnvironmentDeletionReturnsNewDeletedVersion(t *testing.T) {
	deleting := deletingEnvironment(t)
	deletedAt := validEnvironmentReservation().CreatedAt.Add(3 * time.Minute)

	deleted, err := deleting.CompleteDeletion(deletedAt)
	if err != nil {
		t.Fatalf("complete Environment deletion: %v", err)
	}

	if deleting.Snapshot().Lifecycle != domain.EnvironmentDeleting {
		t.Fatalf("original lifecycle = %q, want %q", deleting.Snapshot().Lifecycle, domain.EnvironmentDeleting)
	}
	snapshot := deleted.Snapshot()
	if snapshot.Lifecycle != domain.EnvironmentDeleted {
		t.Errorf("deleted lifecycle = %q, want %q", snapshot.Lifecycle, domain.EnvironmentDeleted)
	}
	if snapshot.Version != 4 {
		t.Errorf("deleted version = %d, want 4", snapshot.Version)
	}
	if snapshot.DeletedAt == nil || !snapshot.DeletedAt.Equal(deletedAt) {
		t.Errorf("deletion time = %v, want %s", snapshot.DeletedAt, deletedAt)
	}
	if !snapshot.UpdatedAt.Equal(deletedAt) {
		t.Errorf("deleted update time = %s, want %s", snapshot.UpdatedAt, deletedAt)
	}
}

func TestCompleteEnvironmentDeletionRejectsInvalidTransition(t *testing.T) {
	if _, err := activeEnvironment(t).CompleteDeletion(validEnvironmentReservation().CreatedAt.Add(2 * time.Minute)); err == nil {
		t.Error("complete deletion of active Environment succeeded, want error")
	}
	if _, err := deletingEnvironment(t).CompleteDeletion(validEnvironmentReservation().CreatedAt); err == nil {
		t.Error("complete Environment deletion in the past succeeded, want error")
	}

	snapshot := deletingEnvironment(t).Snapshot()
	runtimeID := "run_01"
	snapshot.CurrentRuntimeID = &runtimeID
	withRuntime, err := domain.RestoreEnvironment(snapshot)
	if err != nil {
		t.Fatalf("restore deleting Environment with Runtime: %v", err)
	}
	if _, err := withRuntime.CompleteDeletion(validEnvironmentReservation().CreatedAt.Add(3 * time.Minute)); err == nil {
		t.Error("complete Environment deletion with current Runtime succeeded, want error")
	}
}

func TestUpdateEnvironmentHealthReturnsNewVersion(t *testing.T) {
	original := activeEnvironment(t)
	updatedAt := validEnvironmentReservation().CreatedAt.Add(2 * time.Minute)

	degraded, err := original.UpdateHealth(domain.EnvironmentHealthDegraded, updatedAt)
	if err != nil {
		t.Fatalf("update Environment health: %v", err)
	}

	if original.Snapshot().Health != domain.EnvironmentHealthUnknown {
		t.Fatalf("original health = %q, want %q", original.Snapshot().Health, domain.EnvironmentHealthUnknown)
	}
	snapshot := degraded.Snapshot()
	if snapshot.Health != domain.EnvironmentHealthDegraded {
		t.Errorf("updated health = %q, want %q", snapshot.Health, domain.EnvironmentHealthDegraded)
	}
	if snapshot.Version != 3 {
		t.Errorf("updated version = %d, want 3", snapshot.Version)
	}
	if !snapshot.UpdatedAt.Equal(updatedAt) {
		t.Errorf("updated time = %s, want %s", snapshot.UpdatedAt, updatedAt)
	}
}

func TestUpdateEnvironmentHealthRejectsInvalidTransition(t *testing.T) {
	active := activeEnvironment(t)
	if _, err := active.UpdateHealth("online", validEnvironmentReservation().CreatedAt.Add(2*time.Minute)); err == nil {
		t.Error("update Environment to unknown health succeeded, want error")
	}
	if _, err := active.UpdateHealth(domain.EnvironmentHealthHealthy, validEnvironmentReservation().CreatedAt); err == nil {
		t.Error("update Environment health in the past succeeded, want error")
	}
	if _, err := deletedEnvironment(t).UpdateHealth(domain.EnvironmentHealthHealthy, validEnvironmentReservation().CreatedAt.Add(4*time.Minute)); err == nil {
		t.Error("update deleted Environment health succeeded, want error")
	}
}

func TestUpdateEnvironmentHealthIsIdempotent(t *testing.T) {
	active := activeEnvironment(t)
	unchanged, err := active.UpdateHealth(domain.EnvironmentHealthUnknown, validEnvironmentReservation().CreatedAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("repeat Environment health: %v", err)
	}
	if unchanged.Snapshot().Version != active.Snapshot().Version {
		t.Errorf("repeated health version = %d, want %d", unchanged.Snapshot().Version, active.Snapshot().Version)
	}
	if !unchanged.Snapshot().UpdatedAt.Equal(active.Snapshot().UpdatedAt) {
		t.Errorf("repeated health update time = %s, want %s", unchanged.Snapshot().UpdatedAt, active.Snapshot().UpdatedAt)
	}
}

func TestAttachRuntimeReturnsNewEnvironmentVersion(t *testing.T) {
	original, err := domain.ReserveEnvironment(validEnvironmentReservation())
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	attachedAt := validEnvironmentReservation().CreatedAt.Add(time.Minute)

	attached, err := original.AttachRuntime(environmentRuntime(t, "run_01"), attachedAt)
	if err != nil {
		t.Fatalf("attach Runtime: %v", err)
	}

	if original.Snapshot().CurrentRuntimeID != nil {
		t.Fatalf("original current Runtime = %v, want none", original.Snapshot().CurrentRuntimeID)
	}
	snapshot := attached.Snapshot()
	if snapshot.CurrentRuntimeID == nil || *snapshot.CurrentRuntimeID != "run_01" {
		t.Errorf("current Runtime = %v, want run_01", snapshot.CurrentRuntimeID)
	}
	if snapshot.Version != 2 {
		t.Errorf("attached version = %d, want 2", snapshot.Version)
	}
	if !snapshot.UpdatedAt.Equal(attachedAt) {
		t.Errorf("attached update time = %s, want %s", snapshot.UpdatedAt, attachedAt)
	}
}

func TestAttachRuntimeEnforcesCurrentRuntimeInvariant(t *testing.T) {
	creating, err := domain.ReserveEnvironment(validEnvironmentReservation())
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	if _, err := creating.AttachRuntime(domain.Runtime{}, validEnvironmentReservation().CreatedAt.Add(time.Minute)); err == nil {
		t.Error("attach zero Runtime succeeded, want error")
	}
	if _, err := creating.AttachRuntime(environmentRuntime(t, "run_01"), validEnvironmentReservation().CreatedAt.Add(-time.Second)); err == nil {
		t.Error("attach Runtime in the past succeeded, want error")
	}
	if _, err := deletingEnvironment(t).AttachRuntime(environmentRuntime(t, "run_01"), validEnvironmentReservation().CreatedAt.Add(3*time.Minute)); err == nil {
		t.Error("attach Runtime to deleting Environment succeeded, want error")
	}

	attached := attachedEnvironment(t)
	if _, err := attached.AttachRuntime(environmentRuntime(t, "run_02"), validEnvironmentReservation().CreatedAt.Add(2*time.Minute)); err == nil {
		t.Error("attach second current Runtime succeeded, want error")
	}
	unchanged, err := attached.AttachRuntime(environmentRuntime(t, "run_01"), validEnvironmentReservation().CreatedAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("repeat Runtime attachment: %v", err)
	}
	if unchanged.Snapshot().Version != attached.Snapshot().Version {
		t.Errorf("repeated attachment version = %d, want %d", unchanged.Snapshot().Version, attached.Snapshot().Version)
	}
}

func TestDetachRuntimeReturnsNewEnvironmentVersion(t *testing.T) {
	original := attachedEnvironment(t)
	retired := retiredEnvironmentRuntime(t, "run_01")
	detachedAt := validEnvironmentReservation().CreatedAt.Add(6 * time.Minute)

	detached, err := original.DetachRuntime(retired, detachedAt)
	if err != nil {
		t.Fatalf("detach Runtime: %v", err)
	}

	if original.Snapshot().CurrentRuntimeID == nil {
		t.Fatal("original current Runtime = none, want run_01")
	}
	snapshot := detached.Snapshot()
	if snapshot.CurrentRuntimeID != nil {
		t.Errorf("detached current Runtime = %v, want none", snapshot.CurrentRuntimeID)
	}
	if snapshot.Version != 3 {
		t.Errorf("detached version = %d, want 3", snapshot.Version)
	}
	if !snapshot.UpdatedAt.Equal(detachedAt) {
		t.Errorf("detached update time = %s, want %s", snapshot.UpdatedAt, detachedAt)
	}
}

func TestDetachRuntimeEnforcesCurrentRuntimeIdentity(t *testing.T) {
	attached := attachedEnvironment(t)
	retired := retiredEnvironmentRuntime(t, "run_01")
	if _, err := attached.DetachRuntime(domain.Runtime{}, validEnvironmentReservation().CreatedAt.Add(6*time.Minute)); err == nil {
		t.Error("detach zero Runtime succeeded, want error")
	}
	if _, err := attached.DetachRuntime(environmentRuntime(t, "run_01"), validEnvironmentReservation().CreatedAt.Add(6*time.Minute)); err == nil {
		t.Error("detach writable Runtime succeeded, want error")
	}
	if _, err := attached.DetachRuntime(retired, validEnvironmentReservation().CreatedAt); err == nil {
		t.Error("detach Runtime in the past succeeded, want error")
	}
	if _, err := attached.DetachRuntime(retiredEnvironmentRuntime(t, "run_02"), validEnvironmentReservation().CreatedAt.Add(6*time.Minute)); err == nil {
		t.Error("detach non-current Runtime succeeded, want error")
	}
	if _, err := deletedEnvironment(t).DetachRuntime(retired, validEnvironmentReservation().CreatedAt.Add(6*time.Minute)); err == nil {
		t.Error("detach Runtime from deleted Environment succeeded, want error")
	}

	detached, err := attached.DetachRuntime(retired, validEnvironmentReservation().CreatedAt.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("detach Runtime: %v", err)
	}
	unchanged, err := detached.DetachRuntime(retired, validEnvironmentReservation().CreatedAt.Add(7*time.Minute))
	if err != nil {
		t.Fatalf("repeat Runtime detachment: %v", err)
	}
	if unchanged.Snapshot().Version != detached.Snapshot().Version {
		t.Errorf("repeated detachment version = %d, want %d", unchanged.Snapshot().Version, detached.Snapshot().Version)
	}
}

func TestEnvironmentTransitionsRejectZeroValue(t *testing.T) {
	var environment domain.Environment
	at := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		transition func() error
	}{
		{name: "activate", transition: func() error { _, err := environment.Activate(at); return err }},
		{name: "begin deletion", transition: func() error { _, err := environment.BeginDeletion(at); return err }},
		{name: "complete deletion", transition: func() error { _, err := environment.CompleteDeletion(at); return err }},
		{name: "update health", transition: func() error { _, err := environment.UpdateHealth(domain.EnvironmentHealthUnknown, at); return err }},
		{name: "attach Runtime", transition: func() error { _, err := environment.AttachRuntime(environmentRuntime(t, "run_01"), at); return err }},
		{name: "detach Runtime", transition: func() error {
			_, err := environment.DetachRuntime(retiredEnvironmentRuntime(t, "run_01"), at)
			return err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.transition(); err == nil {
				t.Fatal("transition of zero-value Environment succeeded, want error")
			}
		})
	}
}

func TestEnvironmentTransitionRejectsVersionOverflow(t *testing.T) {
	snapshot := validEnvironmentSnapshot()
	snapshot.Version = int64(9223372036854775807)
	environment, err := domain.RestoreEnvironment(snapshot)
	if err != nil {
		t.Fatalf("restore Environment: %v", err)
	}

	if _, err := environment.Activate(snapshot.UpdatedAt.Add(time.Second)); err == nil {
		t.Fatal("activate Environment at maximum version succeeded, want error")
	}
}

func validEnvironmentSnapshot() domain.EnvironmentSnapshot {
	reservation := validEnvironmentReservation()
	return domain.EnvironmentSnapshot{
		ID:                     reservation.ID,
		OwnerUserID:            reservation.OwnerUserID,
		Name:                   reservation.Name,
		Slug:                   reservation.Slug,
		Lifecycle:              domain.EnvironmentCreating,
		Health:                 domain.EnvironmentHealthUnknown,
		Region:                 reservation.Region,
		AvailabilityZone:       reservation.AvailabilityZone,
		RuntimePreset:          reservation.RuntimePreset,
		PinnedProfileVersionID: reservation.PinnedProfileVersionID,
		AutoStopPolicyID:       reservation.AutoStopPolicyID,
		CreatedAt:              reservation.CreatedAt,
		UpdatedAt:              reservation.CreatedAt,
		Version:                1,
	}
}

func validEnvironmentReservation() domain.EnvironmentReservation {
	return domain.EnvironmentReservation{
		ID:                     "env_01",
		OwnerUserID:            "usr_01",
		Name:                   "api-dev",
		Slug:                   "api-dev",
		Region:                 "us-east-1",
		AvailabilityZone:       "us-east-1a",
		RuntimePreset:          "standard",
		PinnedProfileVersionID: "prv_01",
		AutoStopPolicyID:       "asp_01",
		CreatedAt:              time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
	}
}

func activeEnvironment(t *testing.T) domain.Environment {
	t.Helper()
	environment, err := domain.ReserveEnvironment(validEnvironmentReservation())
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	active, err := environment.Activate(validEnvironmentReservation().CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("activate Environment: %v", err)
	}
	return active
}

func attachedEnvironment(t *testing.T) domain.Environment {
	t.Helper()
	environment, err := domain.ReserveEnvironment(validEnvironmentReservation())
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	attached, err := environment.AttachRuntime(environmentRuntime(t, "run_01"), validEnvironmentReservation().CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("attach Runtime: %v", err)
	}
	return attached
}

func environmentRuntime(t *testing.T, id string) domain.Runtime {
	t.Helper()
	reservation := validEnvironmentReservation()
	runtime, err := domain.ReserveRuntime(domain.RuntimeReservation{
		ID: id, EnvironmentID: reservation.ID, Sequence: 1, RuntimePreset: reservation.RuntimePreset,
		Region: reservation.Region, AvailabilityZone: reservation.AvailabilityZone,
		ImageVersion: "ubuntu-2026-07-13", CreatedAt: reservation.CreatedAt,
	})
	if err != nil {
		t.Fatalf("reserve Runtime fixture: %v", err)
	}
	return runtime
}

func retiredEnvironmentRuntime(t *testing.T, id string) domain.Runtime {
	t.Helper()
	createdAt := validEnvironmentReservation().CreatedAt
	runtime, err := environmentRuntime(t, id).Provision("i-"+id, createdAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("provision Runtime fixture: %v", err)
	}
	runtime, err = runtime.BeginStart(createdAt.Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("start Runtime fixture: %v", err)
	}
	runtime, err = runtime.MarkReady(domain.RuntimeReadinessObservation{
		ProviderInstanceRef: "i-" + id, BootID: "boot-" + id, PrivateAddress: "10.0.0.4",
		ExpectedVersion: runtime.Snapshot().Version, ObservedAt: createdAt.Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ready Runtime fixture: %v", err)
	}
	runtime, err = runtime.BeginReplacement(createdAt.Add(4 * time.Minute))
	if err != nil {
		t.Fatalf("replace Runtime fixture: %v", err)
	}
	runtime, err = runtime.Retire(domain.RuntimeStateObservation{
		ProviderInstanceRef: "i-" + id, ExpectedVersion: runtime.Snapshot().Version,
		ObservedAt: createdAt.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("retire Runtime fixture: %v", err)
	}
	return runtime
}

func deletingEnvironment(t *testing.T) domain.Environment {
	t.Helper()
	deleting, err := activeEnvironment(t).BeginDeletion(validEnvironmentReservation().CreatedAt.Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("begin Environment deletion: %v", err)
	}
	return deleting
}

func deletedEnvironment(t *testing.T) domain.Environment {
	t.Helper()
	deleted, err := deletingEnvironment(t).CompleteDeletion(validEnvironmentReservation().CreatedAt.Add(3 * time.Minute))
	if err != nil {
		t.Fatalf("complete Environment deletion: %v", err)
	}
	return deleted
}
