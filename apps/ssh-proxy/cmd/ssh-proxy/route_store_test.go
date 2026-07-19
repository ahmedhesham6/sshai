package main

import (
	"context"
	"errors"
	"testing"

	sshproxy "github.com/ahmedhesham6/sshai/apps/ssh-proxy"
	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

func TestPostgresRouteStoreRequiresCurrentBootReadyRow(t *testing.T) {
	for _, test := range []struct {
		name      string
		values    []any
		err       error
		wantRoute string
		wantErr   error
	}{
		{name: "ready current boot", values: []any{"runtime-1", "ready", "10.0.0.4", "boot-1", int64(9)}, wantRoute: "10.0.0.4:22"},
		{name: "starting ignores stale address", values: []any{"runtime-1", "starting", "10.0.0.3", "boot-stale", int64(8)}, wantErr: sshproxy.ErrRuntimeNotReady},
		{name: "ready without boot is not routable", values: []any{"runtime-1", "ready", "10.0.0.4", nil, int64(9)}, wantErr: sshproxy.ErrRuntimeNotReady},
		{name: "terminal Runtime error", values: []any{"runtime-1", "error", nil, nil, int64(10)}, wantErr: sshproxy.ErrRuntimeStartFailed},
		{name: "foreign Environment", err: pgx.ErrNoRows, wantErr: sshproxy.ErrEnvironmentNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := postgresRouteStore{queries: queryerStub{row: rowStub{values: test.values, err: test.err}}, region: "eu-central-1"}
			route, err := store.ResolveSSH(context.Background(), auth.Subject{WorkOSUserID: "user-1"}, "env-1")
			if !errors.Is(err, test.wantErr) || route.PrivateAddress != test.wantRoute {
				t.Fatalf("route = %#v, error:%v", route, err)
			}
			if test.wantRoute != "" && (route.RuntimeID != "runtime-1" || route.BootID != "boot-1") {
				t.Fatalf("route identity = %#v", route)
			}
		})
	}
}

func TestPostgresRouteStoreMapsEveryDomainRuntimeStatus(t *testing.T) {
	for _, status := range domain.RuntimeStatuses() {
		status := status
		t.Run(string(status), func(t *testing.T) {
			store := postgresRouteStore{
				queries: queryerStub{row: rowStub{values: []any{"runtime-1", string(status), "10.0.0.4", "boot-1", int64(9)}}},
				region:  "eu-central-1",
			}
			route, err := store.ResolveSSH(context.Background(), auth.Subject{WorkOSUserID: "user-1"}, "env-1")
			switch status {
			case domain.RuntimeReady:
				if err != nil || route.PrivateAddress != "10.0.0.4:22" {
					t.Fatalf("ready mapping = route:%#v error:%v", route, err)
				}
			case domain.RuntimeError:
				if !errors.Is(err, sshproxy.ErrRuntimeStartFailed) {
					t.Fatalf("error mapping = %v", err)
				}
			case domain.RuntimeAbsent, domain.RuntimeProvisioning, domain.RuntimeStarting,
				domain.RuntimeStopping, domain.RuntimeStopped, domain.RuntimeReplacing:
				if !errors.Is(err, sshproxy.ErrRuntimeNotReady) {
					t.Fatalf("not-ready mapping = %v", err)
				}
			default:
				t.Fatalf("domain Runtime status %q has no proxy mapping", status)
			}
		})
	}
}

func TestPostgresRouteStoreReturnsVersionedBootAttempt(t *testing.T) {
	store := postgresRouteStore{
		queries: queryerStub{row: rowStub{values: []any{"runtime-1", int64(12), "starting"}}},
		region:  "eu-central-1",
	}
	attempt, err := store.CurrentBootAttempt(context.Background(), "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if attempt.RuntimeID != "runtime-1" || attempt.RuntimeVersion != 12 || attempt.RuntimeStatus != "starting" {
		t.Fatalf("boot attempt = %#v", attempt)
	}
}

type queryerStub struct{ row pgx.Row }

func (stub queryerStub) QueryRow(context.Context, string, ...any) pgx.Row { return stub.row }

type rowStub struct {
	values []any
	err    error
}

func (row rowStub) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(destinations) != len(row.values) {
		return errors.New("unexpected scan width")
	}
	for index, value := range row.values {
		switch destination := destinations[index].(type) {
		case **string:
			if value == nil {
				*destination = nil
				continue
			}
			copy := value.(string)
			*destination = &copy
		case **int64:
			if value == nil {
				*destination = nil
				continue
			}
			copy := value.(int64)
			*destination = &copy
		case *string:
			*destination = value.(string)
		case *int64:
			*destination = value.(int64)
		default:
			return errors.New("unexpected scan destination")
		}
	}
	return nil
}
