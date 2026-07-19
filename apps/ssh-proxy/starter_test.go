package sshproxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sshproxy "github.com/ahmedhesham6/sshai/apps/ssh-proxy"
)

func TestControlPlaneRuntimeStarterReturnsTypedActiveOperationConflict(t *testing.T) {
	const bearer = "FORWARDED_BEARER"
	var idempotencyKey string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/environments/env-1/start" {
			t.Errorf("start request = %s %s", request.Method, request.URL.Path)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer "+bearer {
			t.Errorf("authorization = %q", authorization)
		}
		idempotencyKey = request.Header.Get("Idempotency-Key")
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(response).Encode(map[string]any{
			"requestId": "request-1",
			"error":     map[string]any{"code": "OPERATION_CONFLICT", "message": "active"},
		})
	}))
	defer server.Close()
	starter, err := sshproxy.NewControlPlaneRuntimeStarter(server.URL, server.Client(), &attemptSource{
		attempt: sshproxy.RuntimeBootAttempt{RuntimeID: "runtime-1", RuntimeVersion: 7, RuntimeStatus: "stopped"},
	})
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := starter.EnsureStarted(context.Background(), bearer, "env-1", "connection-1")
	if !errors.Is(err, sshproxy.ErrRuntimeOperationConflict) || operationID != "" {
		t.Fatalf("active start = Operation:%q error:%v", operationID, err)
	}
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 40 {
		t.Fatalf("idempotency key = %q", idempotencyKey)
	}
}

func TestControlPlaneRuntimeStarterReturnsTypedSettlingRuntimeState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(response).Encode(map[string]any{
			"requestId": "request-1",
			"error":     map[string]any{"code": "RUNTIME_COMMAND_INVALID_STATE", "message": "settling"},
		})
	}))
	defer server.Close()
	starter, err := sshproxy.NewControlPlaneRuntimeStarter(server.URL, server.Client(), &attemptSource{
		attempt: sshproxy.RuntimeBootAttempt{RuntimeID: "runtime-1", RuntimeVersion: 8, RuntimeStatus: "starting"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if operationID, err := starter.EnsureStarted(context.Background(), "bearer", "env-1", "connection-1"); !errors.Is(err, sshproxy.ErrRuntimeCommandInvalidState) || operationID != "" {
		t.Fatalf("settling start = Operation:%q error:%v", operationID, err)
	}
}

func TestControlPlaneRuntimeStarterKeyIsStableWithinConnectionAndFreshAcrossConnections(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		keys = append(keys, request.Header.Get("Idempotency-Key"))
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(response).Encode(map[string]any{"operation": map[string]any{"id": "operation-1"}})
	}))
	defer server.Close()
	source := &attemptSource{attempt: sshproxy.RuntimeBootAttempt{RuntimeID: "runtime-1", RuntimeVersion: 7, RuntimeStatus: "stopped"}}
	starter, err := sshproxy.NewControlPlaneRuntimeStarter(server.URL, server.Client(), source)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := starter.EnsureStarted(context.Background(), "bearer", "env-1", "connection-1"); err != nil {
			t.Fatal(err)
		}
	}
	source.attempt.RuntimeVersion++
	source.attempt.RuntimeStatus = "starting"
	if _, err := starter.EnsureStarted(context.Background(), "bearer", "env-1", "connection-1"); err != nil {
		t.Fatal(err)
	}
	source.attempt.RuntimeVersion--
	source.attempt.RuntimeStatus = "stopped"
	if _, err := starter.EnsureStarted(context.Background(), "bearer", "env-1", "connection-2"); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 4 || keys[0] != keys[1] || keys[1] != keys[2] || keys[2] == keys[3] {
		t.Fatalf("start keys = %q", keys)
	}
}

type attemptSource struct {
	attempt sshproxy.RuntimeBootAttempt
	err     error
}

func (source *attemptSource) CurrentBootAttempt(context.Context, string) (sshproxy.RuntimeBootAttempt, error) {
	return source.attempt, source.err
}
