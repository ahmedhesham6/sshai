package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestStoreRegistersSSHKeyWithExactCanonicalReplay(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	ensureSSHKeyOwner(t, ctx, store, "user-1", createdAt)

	first := newSSHKey(t, "ssh-key-1", "user-1", "Laptop", createdAt)
	registered, err := store.RegisterSSHKey(ctx, first, "register-key-1")
	if err != nil {
		t.Fatalf("register SSH Key: %v", err)
	}
	if got := registered.Snapshot(); got.ID != "ssh-key-1" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("registered SSH Key = %#v", got)
	}

	replay := newSSHKey(t, "ssh-key-unused", "user-1", "Laptop", createdAt.Add(time.Minute))
	registered, err = store.RegisterSSHKey(ctx, replay, "register-key-1")
	if err != nil {
		t.Fatalf("replay SSH Key: %v", err)
	}
	if got := registered.Snapshot(); got.ID != "ssh-key-1" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("replayed SSH Key = %#v", got)
	}
	registered, err = store.RegisterSSHKey(ctx, replay, "register-key-2")
	if err != nil {
		t.Fatalf("repeat SSH Key with a new idempotency key: %v", err)
	}
	if got := registered.Snapshot(); got.ID != "ssh-key-1" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("content-addressed SSH Key replay = %#v", got)
	}

	conflict := newSSHKey(t, "ssh-key-conflict", "user-1", "Different label", createdAt.Add(2*time.Minute))
	if _, err := store.RegisterSSHKey(ctx, conflict, "register-key-1"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("conflicting SSH Key registration error = %v", err)
	}
	differentPublicKey := newSSHKeyWithPublicKey(t, "ssh-key-other", "user-1", "Laptop",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGCSduHpnOSVyxOQIIolbJKCG5nodU5mX1/7l4sj1u9z", createdAt.Add(3*time.Minute))
	if _, err := store.RegisterSSHKey(ctx, differentPublicKey, "register-key-1"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("cross-fingerprint idempotency conflict error = %v", err)
	}
}

func TestStoreConvergesConcurrentExactSSHKeyRegistration(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	ensureSSHKeyOwner(t, ctx, store, "user-1", createdAt)

	start := make(chan struct{})
	results := make(chan struct {
		key domain.SSHKey
		err error
	}, 2)
	for index, id := range []string{"ssh-key-1", "ssh-key-2"} {
		candidate := newSSHKey(t, id, "user-1", "Laptop", createdAt.Add(time.Duration(index)*time.Minute))
		go func() {
			<-start
			key, err := store.RegisterSSHKey(ctx, candidate, "register-key-"+string(rune('1'+index)))
			results <- struct {
				key domain.SSHKey
				err error
			}{key: key, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent registration errors = (%v, %v)", first.err, second.err)
	}
	firstSnapshot, secondSnapshot := first.key.Snapshot(), second.key.Snapshot()
	if firstSnapshot.ID != secondSnapshot.ID || !firstSnapshot.CreatedAt.Equal(secondSnapshot.CreatedAt) {
		t.Fatalf("concurrent registrations diverged: %#v != %#v", firstSnapshot, secondSnapshot)
	}
	for index, key := range []string{"register-key-1", "register-key-2"} {
		replayed, err := store.RegisterSSHKey(ctx, newSSHKey(t, "unused", "user-1", "Laptop", createdAt.Add(time.Hour)), key)
		if err != nil {
			t.Fatalf("replay concurrent registration %d: %v", index, err)
		}
		if replayed.Snapshot().ID != firstSnapshot.ID {
			t.Fatalf("replayed registration %d = %#v", index, replayed.Snapshot())
		}
	}
}

func TestStoreListsOnlyActiveOwnedSSHKeys(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	ensureSSHKeyOwner(t, ctx, store, "user-1", createdAt)
	ensureSSHKeyOwner(t, ctx, store, "user-2", createdAt)

	first := newSSHKey(t, "ssh-key-1", "user-1", "Laptop", createdAt)
	second := newSSHKeyWithPublicKey(t, "ssh-key-2", "user-1", "Desktop",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGCSduHpnOSVyxOQIIolbJKCG5nodU5mX1/7l4sj1u9z", createdAt.Add(time.Minute))
	foreign := newSSHKey(t, "ssh-key-foreign", "user-2", "Foreign", createdAt)
	for index, candidate := range []domain.SSHKey{first, second, foreign} {
		if _, err := store.RegisterSSHKey(ctx, candidate, "register-key-"+string(rune('1'+index))); err != nil {
			t.Fatalf("register SSH Key %d: %v", index, err)
		}
	}

	keys, err := store.ListActiveOwnedSSHKeys(ctx, "user-1")
	if err != nil {
		t.Fatalf("list active owned SSH Keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("active owned SSH Key count = %d", len(keys))
	}
	if keys[0].Snapshot().ID != "ssh-key-1" || keys[1].Snapshot().ID != "ssh-key-2" {
		t.Fatalf("active owned SSH Keys = %#v", []domain.SSHKeySnapshot{keys[0].Snapshot(), keys[1].Snapshot()})
	}
}

func TestStoreRevokesOwnedSSHKeyIdempotentlyWithoutDisclosingForeignKeys(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	createdAt := time.Date(2026, time.July, 13, 16, 0, 0, 0, time.UTC)
	ensureSSHKeyOwner(t, ctx, store, "user-1", createdAt)
	ensureSSHKeyOwner(t, ctx, store, "user-2", createdAt)
	first := newSSHKey(t, "ssh-key-1", "user-1", "Laptop", createdAt)
	second := newSSHKeyWithPublicKey(t, "ssh-key-2", "user-1", "Desktop",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGCSduHpnOSVyxOQIIolbJKCG5nodU5mX1/7l4sj1u9z", createdAt.Add(time.Minute))
	if _, err := store.RegisterSSHKey(ctx, first, "register-key-1"); err != nil {
		t.Fatalf("register first SSH Key: %v", err)
	}
	if _, err := store.RegisterSSHKey(ctx, second, "register-key-2"); err != nil {
		t.Fatalf("register second SSH Key: %v", err)
	}

	if err := store.RevokeOwnedSSHKey(ctx, "user-1", "ssh-key-1", "register-key-1", createdAt.Add(2*time.Minute)); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("registration/revocation idempotency conflict error = %v", err)
	}
	if err := store.RevokeOwnedSSHKey(ctx, "user-2", "ssh-key-1", "revoke-key-foreign", createdAt.Add(2*time.Minute)); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("foreign SSH Key revocation error = %v", err)
	}
	revokedAt := createdAt.Add(3 * time.Minute)
	if err := store.RevokeOwnedSSHKey(ctx, "user-1", "ssh-key-1", "revoke-key-1", revokedAt); err != nil {
		t.Fatalf("revoke owned SSH Key: %v", err)
	}
	if err := store.RevokeOwnedSSHKey(ctx, "user-1", "ssh-key-1", "revoke-key-1", revokedAt.Add(time.Minute)); err != nil {
		t.Fatalf("replay SSH Key revocation: %v", err)
	}
	if err := store.RevokeOwnedSSHKey(ctx, "user-1", "ssh-key-2", "revoke-key-1", revokedAt.Add(time.Minute)); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("conflicting SSH Key revocation error = %v", err)
	}

	replayed, err := store.RegisterSSHKey(ctx, newSSHKey(t, "unused", "user-1", "Laptop", revokedAt.Add(time.Hour)), "register-key-1")
	if err != nil {
		t.Fatalf("replay revoked SSH Key registration: %v", err)
	}
	if got := replayed.Snapshot().RevokedAt; got == nil || !got.Equal(revokedAt) {
		t.Fatalf("replayed SSH Key revocation = %v", got)
	}
	if _, err := store.RegisterSSHKey(ctx, newSSHKey(t, "unused-new-key", "user-1", "Laptop", revokedAt.Add(time.Hour)), "register-key-new"); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("fresh registration of revoked fingerprint error = %v", err)
	}
	active, err := store.ListActiveOwnedSSHKeys(ctx, "user-1")
	if err != nil {
		t.Fatalf("list active SSH Keys after revocation: %v", err)
	}
	if len(active) != 1 || active[0].Snapshot().ID != "ssh-key-2" {
		t.Fatalf("active SSH Keys after revocation = %#v", active)
	}
}

func ensureSSHKeyOwner(t *testing.T, ctx context.Context, store *dbstore.Store, ownerID string, at time.Time) {
	t.Helper()
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID: ownerID, WorkOSUserID: "workos-" + ownerID, DefaultRegion: "us-east-1", ObservedAt: at,
	}); err != nil {
		t.Fatalf("ensure SSH Key owner: %v", err)
	}
}

func newSSHKey(t *testing.T, id, ownerID, label string, createdAt time.Time) domain.SSHKey {
	t.Helper()
	return newSSHKeyWithPublicKey(t, id, ownerID, label,
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMzdhbPIA9osmLQz0iTvx/VNJP8fjiD3wfl9LSn2d92", createdAt)
}

func newSSHKeyWithPublicKey(t *testing.T, id, ownerID, label, publicKey string, createdAt time.Time) domain.SSHKey {
	t.Helper()
	key, err := domain.RegisterSSHKey(domain.SSHKeyRegistration{
		ID: id, OwnerUserID: ownerID, Label: label,
		PublicKey: publicKey, CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("register domain SSH Key: %v", err)
	}
	return key
}
