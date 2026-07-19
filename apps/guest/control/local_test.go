package control

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	capsuleoci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
)

func TestGrantHTTPErrorClassification(t *testing.T) {
	for _, test := range []struct {
		status    int
		transient bool
	}{
		{status: http.StatusBadRequest},
		{status: http.StatusNotFound},
		{status: http.StatusForbidden, transient: true},
		{status: http.StatusRequestTimeout, transient: true},
		{status: http.StatusTooManyRequests, transient: true},
		{status: http.StatusInternalServerError, transient: true},
	} {
		if got := (grantHTTPError{status: test.status}).Transient(); got != test.transient {
			t.Errorf("HTTP %d transient = %v, want %v", test.status, got, test.transient)
		}
	}
}

func TestExpiredSerializedGrantIsTransientAndFreshAttemptReplacesIt(t *testing.T) {
	key := "owner/user-1/blobs/sha256/example"
	expired, err := newReadGrantProvider(map[string]ReadGrant{
		key: {URL: "https://capsules.example/expired", ExpiresAt: time.Now().Add(-time.Minute)},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = expired.Grant(context.Background(), capsuleoci.GrantRequest{Key: key, Operation: capsuleoci.GrantRead})
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || !classified.Transient() {
		t.Fatalf("expired grant error = %T %v, want transient", err, err)
	}

	fresh, err := newReadGrantProvider(map[string]ReadGrant{
		key: {URL: "https://capsules.example/fresh", ExpiresAt: time.Now().Add(time.Minute)},
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := fresh.Grant(context.Background(), capsuleoci.GrantRequest{Key: key, Operation: capsuleoci.GrantRead})
	if err != nil || grant.Read == nil {
		t.Fatalf("fresh retry grant = %#v, %v", grant, err)
	}
}
