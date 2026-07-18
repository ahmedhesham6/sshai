package controlplane_test

import (
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db"
)

// paginateFake emulates the Store's keyset pagination purely in memory, so
// handler tests can exercise cursor/page-size behavior against a reader
// fake without a real Postgres-backed Store. items must already be ordered
// by (createdAt, id) ascending -- the same order every real keyset query in
// libs/db produces -- and keyOf extracts that (createdAt, id) tuple for a
// given element.
func paginateFake[T any](items []T, keyOf func(T) (time.Time, string), cursor *db.Cursor, pageSize int) ([]T, *db.Cursor) {
	start := 0
	if cursor != nil {
		start = len(items)
		for index, item := range items {
			createdAt, id := keyOf(item)
			if createdAt.Equal(cursor.CreatedAt) && id == cursor.ID {
				start = index + 1
				break
			}
		}
	}
	if start >= len(items) {
		return []T{}, nil
	}
	end := start + pageSize
	if end >= len(items) {
		return items[start:], nil
	}
	createdAt, id := keyOf(items[end-1])
	return items[start:end], &db.Cursor{CreatedAt: createdAt, ID: id}
}

// requireOwner reports a distinguishable error when ownerID does not match
// expectedOwnerID, the pattern apps/workflows/cmd/workflows/deps_test.go's
// capsuleResolverStub uses. Every reader fake in this package calls it
// first, so a regression that threads the wrong owner ID through a Get/List
// call fails the test loudly instead of silently returning data scoped to
// the wrong owner.
func requireOwner(method, ownerID, expectedOwnerID string) error {
	if ownerID != expectedOwnerID {
		return fmt.Errorf("%s() ownerID = %q, want %q", method, ownerID, expectedOwnerID)
	}
	return nil
}
