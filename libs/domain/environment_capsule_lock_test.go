package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestEnvironmentDefaultsUpgradePolicyToManual(t *testing.T) {
	environment, err := domain.ReserveEnvironment(validEnvironmentReservation())
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	if got := environment.Snapshot().UpgradePolicy; got != domain.UpgradeManual {
		t.Fatalf("reserved Environment upgrade policy = %q, want manual", got)
	}

	snapshot := validEnvironmentSnapshot()
	snapshot.UpgradePolicy = ""
	restored, err := domain.RestoreEnvironment(snapshot)
	if err != nil {
		t.Fatalf("restore Environment with legacy empty policy: %v", err)
	}
	if got := restored.Snapshot().UpgradePolicy; got != domain.UpgradeManual {
		t.Fatalf("restored Environment upgrade policy = %q, want manual", got)
	}
}

func TestEnvironmentRejectsInvalidUpgradePolicy(t *testing.T) {
	snapshot := validEnvironmentSnapshot()
	snapshot.UpgradePolicy = domain.UpgradePolicy("always")
	if _, err := domain.RestoreEnvironment(snapshot); err == nil {
		t.Fatal("restored Environment with invalid upgrade policy")
	}
}

func TestEnvironmentPinsCapsuleLockOnlyOnceUnlessExplicitlyRepinned(t *testing.T) {
	createdAt := validEnvironmentReservation().CreatedAt
	environment, err := domain.ReserveEnvironment(validEnvironmentReservation())
	if err != nil {
		t.Fatalf("reserve Environment: %v", err)
	}
	first := capsuleLockFixture(t, "lock-1", "env_01", createdAt)
	second := capsuleLockFixture(t, "lock-2", "env_01", createdAt.Add(time.Minute))
	foreign := capsuleLockFixture(t, "lock-foreign", "env_other", createdAt.Add(2*time.Minute))

	pinned, err := environment.PinCapsuleLock(first, domain.UpgradeNotify, createdAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("pin first Capsule Lock: %v", err)
	}
	if pinned.Snapshot().CapsuleLockID == nil || *pinned.Snapshot().CapsuleLockID != "lock-1" || pinned.Snapshot().UpgradePolicy != domain.UpgradeNotify {
		t.Fatalf("pinned Environment = %#v", pinned.Snapshot())
	}
	if _, err := pinned.PinCapsuleLock(second, domain.UpgradeAutoSafe, createdAt.Add(2*time.Minute)); err == nil {
		t.Fatal("second Capsule Lock pin succeeded without explicit re-pin")
	}
	if _, err := pinned.RepinCapsuleLock(foreign, domain.UpgradeAutoSafe, createdAt.Add(2*time.Minute)); err == nil {
		t.Fatal("foreign Capsule Lock re-pin succeeded")
	}
	repinned, err := pinned.RepinCapsuleLock(second, domain.UpgradeAutoSafe, createdAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("explicitly re-pin Capsule Lock: %v", err)
	}
	if repinned.Snapshot().CapsuleLockID == nil || *repinned.Snapshot().CapsuleLockID != "lock-2" || repinned.Snapshot().UpgradePolicy != domain.UpgradeAutoSafe {
		t.Fatalf("repinned Environment = %#v", repinned.Snapshot())
	}
}

func capsuleLockFixture(t *testing.T, id, environmentID string, createdAt time.Time) domain.CapsuleLock {
	t.Helper()
	digest := "sha256:" + strings.Repeat("a", 64)
	lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
		ID: id, EnvironmentID: environmentID, ProfileVersionID: "prv_01", ProjectCapsuleDigest: digest,
		Capsules: []domain.LockedCapsule{{Ref: "owner/usr_01/capsule@" + digest, Digest: digest}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:editor": {ID: "config:editor", Type: domain.ComponentConfig, CapsuleDigest: digest, ComponentDigest: digest, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative},
		},
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("create Capsule Lock fixture: %v", err)
	}
	return lock
}
