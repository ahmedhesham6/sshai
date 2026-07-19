package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	sshproxy "github.com/ahmedhesham6/sshai/apps/ssh-proxy"
	"github.com/ahmedhesham6/sshai/libs/auth"
)

func TestLoadConfigUsesBoundedProductionDefaults(t *testing.T) {
	for name, value := range map[string]string{
		"DATABASE_URL": "postgres://route-db", "CONTROL_PLANE_URL": "https://api.example",
		"REGION": "eu-central-1", "WORKOS_CLIENT_ID": "client-1",
	} {
		t.Setenv(name, value)
	}
	for _, name := range []string{
		"LISTEN_ADDR", "WORKOS_ISSUER", "WORKOS_JWKS_URL", "DIAL_TIMEOUT", "IDLE_TIMEOUT",
		"START_WAIT_TIMEOUT", "START_SETTLE_TIMEOUT", "ROUTE_POLL_INTERVAL", "CONTROL_WRITE_TIMEOUT", "BRIDGE_BUFFER_BYTES",
	} {
		t.Setenv(name, "")
	}
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.listenAddress != ":8082" || config.startTimeout != 10*time.Minute || config.settleTimeout != 15*time.Second || config.pollInterval != 2*time.Second ||
		config.dialTimeout != 10*time.Second || config.idleTimeout != 2*time.Minute || config.bufferBytes != 32*1024 {
		t.Fatalf("proxy defaults = %#v", config)
	}
}

func TestBuildHandlerNeedsNoLiveDependencies(t *testing.T) {
	handler, err := buildHandler(
		context.Background(), config{},
		verifierStub{}, intentStub{}, routeStub{}, starterStub{}, dialerStub{},
	)
	if err != nil || handler == nil {
		t.Fatalf("build handler = handler:%v error:%v", handler, err)
	}
}

type verifierStub struct{}

func (verifierStub) Verify(context.Context, string) (auth.Subject, error) {
	return auth.Subject{WorkOSUserID: "user-1"}, nil
}

type routeStub struct{}

type intentStub struct{}

func (intentStub) Consume(context.Context, auth.Subject, string, string) (sshproxy.ConnectionIntentAttempt, error) {
	return sshproxy.ConnectionIntentAttempt{}, nil
}

func (routeStub) ResolveSSH(context.Context, auth.Subject, string) (sshproxy.EnvironmentSSHRoute, error) {
	return sshproxy.EnvironmentSSHRoute{RuntimeID: "runtime-1", BootID: "boot-1", PrivateAddress: "10.0.0.4:22"}, nil
}

type starterStub struct{}

func (starterStub) EnsureStarted(context.Context, string, string, string) (string, error) {
	return "", errors.New("not used")
}

type dialerStub struct{}

func (dialerStub) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("not used")
}
