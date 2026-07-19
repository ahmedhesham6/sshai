package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/ahmedhesham6/sshai/libs/connection"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/coder/websocket"
)

func TestCLISSHProxyLoadsPrivateSessionAndKeepsStdoutOpaque(t *testing.T) {
	configDirectory := t.TempDir()
	if err := persistCredentials(configDirectory, loginCredentials{
		accessToken: testAccessToken(t, time.Now().Add(time.Hour)), refreshToken: "REFRESH_SECRET",
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(configDirectory, "auth", "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]string
	if err := json.Unmarshal(content, &stored); err != nil {
		t.Fatal(err)
	}
	accessToken := stored["access_token"]
	server := newProxyTracerServer(t, accessToken, "env_01", func(ctx context.Context, connection *websocket.Conn) {
		_, content, err := connection.Read(ctx)
		if err != nil || string(content) != "client-bytes" {
			t.Errorf("client stream = content:%q error:%v", content, err)
			return
		}
		_ = connection.Write(ctx, websocket.MessageBinary, []byte("server-bytes"))
		_ = connection.Close(websocket.StatusNormalClosure, "")
	})
	defer server.Close()
	var stdout bytes.Buffer
	application := cli{
		output: &stdout, input: strings.NewReader("client-bytes"), clientID: "client_public_01",
		controlPlaneURL: server.URL + "/v1", httpClient: server.Client(), now: time.Now,
		configDirectory:  func() (string, error) { return configDirectory, nil },
		newRefreshClient: func(string) (tokenRefresher, error) { return &singleUseRefreshFake{}, nil },
		newAttempt:       func() (string, error) { return "attempt-01", nil },
	}
	if err := application.run(context.Background(), []string{"ssh-proxy", "--environment", "env_01"}); err != nil {
		t.Fatalf("CLI SSH proxy: %v", err)
	}
	if stdout.String() != "server-bytes" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestCLISSHProxyRequiresExactFlagsAndPublicConfiguration(t *testing.T) {
	base := cli{
		output: io.Discard, input: bytes.NewReader(nil), clientID: "client_public_01",
		controlPlaneURL: "https://api.example/v1", httpClient: http.DefaultClient, now: time.Now,
		configDirectory: func() (string, error) { return t.TempDir(), nil },
		newRefreshClient: func(string) (tokenRefresher, error) {
			pair, _ := auth.NewTokenPair("access", "refresh")
			return &singleUseRefreshFake{rotated: pair}, nil
		},
		newAttempt: func() (string, error) { return "attempt-01", nil },
	}
	for _, test := range []struct {
		name string
		args []string
		edit func(*cli)
	}{
		{name: "missing Environment", args: []string{"ssh-proxy"}},
		{name: "extra argument", args: []string{"ssh-proxy", "--environment", "env_01", "extra"}},
		{name: "missing public client ID", args: []string{"ssh-proxy", "--environment", "env_01"}, edit: func(app *cli) { app.clientID = "" }},
		{name: "missing control plane", args: []string{"ssh-proxy", "--environment", "env_01"}, edit: func(app *cli) { app.controlPlaneURL = "" }},
		{name: "insecure control plane", args: []string{"ssh-proxy", "--environment", "env_01"}, edit: func(app *cli) { app.controlPlaneURL = "http://api.example/v1" }},
		{name: "control plane empty query", args: []string{"ssh-proxy", "--environment", "env_01"}, edit: func(app *cli) { app.controlPlaneURL = "https://api.example/v1?" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			application := base
			if test.edit != nil {
				test.edit(&application)
			}
			if err := application.run(context.Background(), test.args); err == nil {
				t.Fatal("CLI accepted invalid SSH proxy invocation")
			}
		})
	}
	if _, err := secureControlPlaneURL("https://api.example/v1?"); err == nil {
		t.Fatal("control plane URL accepted an empty query delimiter")
	}
}

func TestConnectionAttemptKeyIsShortDeterministicAndAttemptScoped(t *testing.T) {
	first, err := connectionAttemptKey("env_01", "attempt-01")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := connectionAttemptKey("env_01", "attempt-01")
	if err != nil {
		t.Fatal(err)
	}
	other, err := connectionAttemptKey("env_01", "attempt-02")
	if err != nil {
		t.Fatal(err)
	}
	if first != replay || first == other || len(first) < 16 || len(first) > 40 {
		t.Fatalf("attempt keys = %q, %q, %q", first, replay, other)
	}
}

func TestSSHProxyCommandUsesFreshBearerForRealHTTPAndWSSBinaryStream(t *testing.T) {
	const token = "ACCESS_SECRET"
	const input = "\x00SSH-2.0-client\r\n\xff"
	const output = "\x00SSH-2.0-server\r\n\xfe"
	var websocketCalls atomic.Int32
	server := newProxyTracerServer(t, token, "env_01", func(ctx context.Context, connection *websocket.Conn) {
		websocketCalls.Add(1)
		messageType, content, err := connection.Read(ctx)
		if err != nil {
			t.Errorf("read client bytes: %v", err)
			return
		}
		if messageType != websocket.MessageBinary || string(content) != input {
			t.Errorf("client frame = type:%v content:%q", messageType, content)
			return
		}
		if err := connection.Write(ctx, websocket.MessageBinary, []byte(output)); err != nil {
			t.Errorf("write server bytes: %v", err)
		}
		_ = connection.Close(websocket.StatusNormalClosure, "")
	})
	defer server.Close()

	var stdout bytes.Buffer
	command := sshProxyCommand{
		controlPlaneURL: server.URL + "/v1",
		httpClient:      server.Client(),
		tokens:          staticAccessTokenSource{token: token},
		attempt:         "attempt-01",
		input:           strings.NewReader(input),
		output:          &stdout,
		now:             time.Now,
	}
	if err := command.run(context.Background(), "env_01"); err != nil {
		t.Fatalf("run SSH proxy: %v", err)
	}
	if stdout.String() != output || websocketCalls.Load() != 1 {
		t.Fatalf("stream = output:%q websocket-calls:%d", stdout.String(), websocketCalls.Load())
	}
}

func TestSSHProxyCommandRendersProgressUntilReadyThenBridgesBinary(t *testing.T) {
	const token = "ACCESS_SECRET"
	const peerMessage = "PEER_CONTROL_TEXT_MUST_NOT_BE_PRINTED"
	server := newProxyControlServer(t, token, "env_01", func(ctx context.Context, socket *websocket.Conn) {
		writeControlFrame(t, ctx, socket, connection.ControlFrame{Type: connection.ControlProgress, Step: "starting-runtime", Message: peerMessage})
		writeControlFrame(t, ctx, socket, connection.ControlFrame{Type: "advisory", Message: "ignored"})
		writeControlFrame(t, ctx, socket, connection.ControlFrame{Type: connection.ControlProgress, Step: "waiting-for-guest", Message: peerMessage})
		writeControlFrame(t, ctx, socket, connection.ControlFrame{Type: connection.ControlProgress, Step: "resolving-route", Message: peerMessage})
		writeControlFrame(t, ctx, socket, connection.ControlFrame{Type: connection.ControlProgress, Step: "future-step", Message: peerMessage})
		writeControlFrame(t, ctx, socket, connection.ControlFrame{Type: connection.ControlReady})
		messageType, content, err := socket.Read(ctx)
		if err != nil || messageType != websocket.MessageBinary || string(content) != "client-bytes" {
			t.Errorf("client stream = type:%v content:%q error:%v", messageType, content, err)
			return
		}
		_ = socket.Write(ctx, websocket.MessageBinary, []byte("server-bytes"))
		_ = socket.Close(websocket.StatusNormalClosure, "")
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	command := sshProxyCommand{
		controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
		tokens: staticAccessTokenSource{token: token}, attempt: "attempt-01",
		input: strings.NewReader("client-bytes"), output: &stdout, errorOutput: &stderr, now: time.Now,
	}
	if err := command.run(context.Background(), "env_01"); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "server-bytes" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	wantProgress := "devm: starting-runtime: Starting Runtime\n" +
		"devm: waiting-for-guest: Waiting for SSH readiness\n" +
		"devm: resolving-route: Resolving the Runtime route\n" +
		"devm: Preparing SSH connection\n"
	if stderr.String() != wantProgress {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantProgress)
	}
}

func TestSSHProxyCommandUsesGenericFailureForUnknownControlStep(t *testing.T) {
	const peerText = "PEER_FAILURE_TEXT_MUST_NOT_BE_PRINTED"
	server := newProxyControlServer(t, "ACCESS_SECRET", "env_01", func(ctx context.Context, socket *websocket.Conn) {
		writeControlFrame(t, ctx, socket, connection.ControlFrame{
			Type: connection.ControlFailed, Step: peerText, Message: peerText,
		})
	})
	defer server.Close()
	command := sshProxyCommand{
		controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
		tokens: staticAccessTokenSource{token: "ACCESS_SECRET"}, attempt: "attempt-01",
		input: bytes.NewReader(nil), output: io.Discard, now: time.Now,
	}
	err := command.run(context.Background(), "env_01")
	message := fmt.Sprint(err)
	if err == nil || !strings.Contains(message, "failed while preparing the Runtime") ||
		!strings.Contains(message, "regional proxy could not prepare the Runtime") || strings.Contains(message, peerText) {
		t.Fatalf("unknown-step failure = %v", err)
	}
}

func TestSSHProxyCommandFailedControlNamesStepAndLeaksNoBearerOrPayload(t *testing.T) {
	const token = "ACCESS_SECRET"
	const payload = "SSH_PAYLOAD_SECRET"
	server := newProxyControlServer(t, token, "env_01", func(ctx context.Context, socket *websocket.Conn) {
		writeControlFrame(t, ctx, socket, connection.ControlFrame{
			Type: connection.ControlProgress, Step: "starting-runtime", Message: "token " + token,
		})
		content := fmt.Sprintf(`{"type":"failed","operationId":%q,"step":"waiting-for-guest","message":%q,"payload":%q}`, token, payload, payload)
		_ = socket.Write(ctx, websocket.MessageText, []byte(content))
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	command := sshProxyCommand{
		controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
		tokens: staticAccessTokenSource{token: token}, attempt: "attempt-01",
		input: strings.NewReader(payload), output: &stdout, errorOutput: &stderr, now: time.Now,
	}
	err := command.run(context.Background(), "env_01")
	message := fmt.Sprint(err)
	if err == nil || !strings.Contains(message, "waiting-for-guest") || !strings.Contains(message, "SSH readiness could not be confirmed") ||
		!strings.Contains(message, "persistent Environment state remains intact") {
		t.Fatalf("failed control error = %v", err)
	}
	if stdout.Len() != 0 || strings.Contains(message, token) || strings.Contains(message, payload) ||
		strings.Contains(stderr.String(), token) || strings.Contains(stderr.String(), payload) {
		t.Fatalf("failed control leaked data: stdout=%q stderr=%q error=%v", stdout.String(), stderr.String(), err)
	}
}

func TestSSHProxyCommandRejectsBinaryBeforeReadyWithoutWritingPayload(t *testing.T) {
	const payload = "SSH_PAYLOAD_SECRET"
	server := newProxyControlServer(t, "ACCESS_SECRET", "env_01", func(ctx context.Context, socket *websocket.Conn) {
		_ = socket.Write(ctx, websocket.MessageBinary, []byte(payload))
	})
	defer server.Close()
	var stdout bytes.Buffer
	command := sshProxyCommand{
		controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
		tokens: staticAccessTokenSource{token: "ACCESS_SECRET"}, attempt: "attempt-01",
		input: bytes.NewReader(nil), output: &stdout, now: time.Now,
	}
	err := command.run(context.Background(), "env_01")
	if err == nil || stdout.Len() != 0 || strings.Contains(fmt.Sprint(err), payload) {
		t.Fatalf("binary-before-ready result = output:%q error:%v", stdout.Bytes(), err)
	}
}

func TestSSHProxyCommandRejectsControlFramesWithoutTypes(t *testing.T) {
	for _, body := range []string{"null", `{}`} {
		t.Run(body, func(t *testing.T) {
			server := newProxyControlServer(t, "ACCESS_SECRET", "env_01", func(ctx context.Context, socket *websocket.Conn) {
				if err := socket.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
					t.Errorf("write empty-type frame: %v", err)
					return
				}
				_, _, _ = socket.Read(ctx)
			})
			defer server.Close()
			command := sshProxyCommand{
				controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
				tokens: staticAccessTokenSource{token: "ACCESS_SECRET"}, attempt: "attempt-01",
				input: bytes.NewReader(nil), output: io.Discard, now: time.Now,
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := command.run(ctx, "env_01")
			if err == nil || !strings.Contains(err.Error(), "control frame type is missing") {
				t.Fatalf("empty-type control frame error = %v", err)
			}
		})
	}
}

func TestThreeConcurrentSSHProxyCommandsRotateOneSessionExactlyOnce(t *testing.T) {
	now := time.Now()
	configDirectory := t.TempDir()
	if err := persistCredentials(configDirectory, loginCredentials{
		accessToken: testAccessToken(t, now.Add(-time.Minute)), refreshToken: "refresh_old",
	}); err != nil {
		t.Fatal(err)
	}
	rotatedAccess := testAccessToken(t, now.Add(time.Hour))
	rotatedPair, err := auth.NewTokenPair(rotatedAccess, "refresh_new")
	if err != nil {
		t.Fatal(err)
	}
	refresher := &singleUseRefreshFake{rotated: rotatedPair}
	server := newProxyTracerServer(t, rotatedAccess, "env_01", func(_ context.Context, connection *websocket.Conn) {
		_ = connection.Close(websocket.StatusNormalClosure, "")
	})
	defer server.Close()
	start := make(chan struct{})
	results := make(chan error, 3)
	var group sync.WaitGroup
	for index := range 3 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			command := sshProxyCommand{
				controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
				tokens:  newTokenSession(configDirectory, refresher, func() time.Time { return now }),
				attempt: fmt.Sprintf("attempt-%02d", index), input: bytes.NewReader(nil), output: io.Discard,
				now: func() time.Time { return now },
			}
			results <- command.run(context.Background(), "env_01")
		}()
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent SSH proxy: %v", err)
		}
	}
	if calls := refresher.callCount(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
	content, err := os.ReadFile(filepath.Join(configDirectory, "auth", "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte("refresh_new")) || bytes.Contains(content, []byte("refresh_old")) {
		t.Fatal("rotated credential pair was not persisted atomically")
	}
}

func TestSSHProxyCommandNotReadyResponseDoesNotExposePayloadOrBearer(t *testing.T) {
	const token = "ACCESS_SECRET"
	const payload = `{"error":{"message":"RUNTIME_PAYLOAD_SECRET"}}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(response, payload)
	}))
	defer server.Close()
	command := sshProxyCommand{
		controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
		tokens: staticAccessTokenSource{token: token}, attempt: "attempt-01",
		input: bytes.NewReader(nil), output: io.Discard, now: time.Now,
	}
	err := command.run(context.Background(), "env_01")
	if err == nil || strings.Contains(fmt.Sprint(err), token) || strings.Contains(fmt.Sprint(err), "RUNTIME_PAYLOAD_SECRET") {
		t.Fatalf("not-ready error = %v", err)
	}
}

func TestSSHProxyCommandRejectsUnsafeConnectionIntentBeforeDial(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	valid := contracts.ConnectionIntent{
		Id: "intent-01", EnvironmentId: "env_01", LogicalHostname: "env_01",
		ProxyUrl: "wss://region.example/v1/environments/env_01/ssh", ExpiresAt: now.Add(time.Minute),
	}
	tests := []struct {
		name   string
		mutate func(*contracts.ConnectionIntent)
	}{
		{name: "missing intent identity", mutate: func(intent *contracts.ConnectionIntent) { intent.Id = "" }},
		{name: "wrong Environment", mutate: func(intent *contracts.ConnectionIntent) { intent.EnvironmentId = "env_02" }},
		{name: "wrong logical host", mutate: func(intent *contracts.ConnectionIntent) { intent.LogicalHostname = "alias" }},
		{name: "expired", mutate: func(intent *contracts.ConnectionIntent) { intent.ExpiresAt = now }},
		{name: "insecure socket", mutate: func(intent *contracts.ConnectionIntent) {
			intent.ProxyUrl = "ws://region.example/v1/environments/env_01/ssh"
		}},
		{name: "embedded credentials", mutate: func(intent *contracts.ConnectionIntent) {
			intent.ProxyUrl = "wss://user:pass@region.example/v1/environments/env_01/ssh"
		}},
		{name: "query", mutate: func(intent *contracts.ConnectionIntent) { intent.ProxyUrl += "?token=secret" }},
		{name: "empty query", mutate: func(intent *contracts.ConnectionIntent) { intent.ProxyUrl += "?" }},
		{name: "fragment", mutate: func(intent *contracts.ConnectionIntent) { intent.ProxyUrl += "#secret" }},
		{name: "wrong regional path", mutate: func(intent *contracts.ConnectionIntent) { intent.ProxyUrl = "wss://region.example/other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			intent := valid
			test.mutate(&intent)
			if _, err := validateConnectionIntent(intent, "env_01", now); err == nil {
				t.Fatal("connection intent validator accepted unsafe response")
			}
			server := connectionIntentServer(t, "ACCESS_SECRET", intent)
			defer server.Close()
			var stdout bytes.Buffer
			command := sshProxyCommand{
				controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
				tokens: staticAccessTokenSource{token: "ACCESS_SECRET"}, attempt: "attempt-01",
				input: strings.NewReader("payload"), output: &stdout, now: func() time.Time { return now },
			}
			err := command.run(context.Background(), "env_01")
			if err == nil || stdout.Len() != 0 {
				t.Fatalf("unsafe intent result = output:%q error:%v", stdout.Bytes(), err)
			}
			if strings.Contains(fmt.Sprint(err), "ACCESS_SECRET") || strings.Contains(fmt.Sprint(err), "token=secret") {
				t.Fatalf("error exposed secret: %v", err)
			}
		})
	}
}

func TestSSHProxyCommandRejectsTextFramesWithoutWritingPayload(t *testing.T) {
	const secretPayload = "SSH_PAYLOAD_SECRET"
	server := newProxyTracerServer(t, "ACCESS_SECRET", "env_01", func(ctx context.Context, connection *websocket.Conn) {
		_ = connection.Write(ctx, websocket.MessageText, []byte(secretPayload))
	})
	defer server.Close()
	var stdout bytes.Buffer
	command := sshProxyCommand{
		controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
		tokens: staticAccessTokenSource{token: "ACCESS_SECRET"}, attempt: "attempt-01",
		input: bytes.NewReader(nil), output: &stdout, now: time.Now,
	}
	err := command.run(context.Background(), "env_01")
	if err == nil || stdout.Len() != 0 || strings.Contains(fmt.Sprint(err), secretPayload) {
		t.Fatalf("text frame result = output:%q error:%v", stdout.Bytes(), err)
	}
}

func TestSSHProxyCommandCancellationStopsOpenStream(t *testing.T) {
	server := newProxyTracerServer(t, "ACCESS_SECRET", "env_01", func(ctx context.Context, connection *websocket.Conn) {
		<-ctx.Done()
	})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := sshProxyCommand{
		controlPlaneURL: server.URL + "/v1", httpClient: server.Client(),
		tokens: staticAccessTokenSource{token: "ACCESS_SECRET"}, attempt: "attempt-01",
		input: bytes.NewReader(nil), output: io.Discard, now: time.Now,
	}
	if err := command.run(ctx, "env_01"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

type staticAccessTokenSource struct {
	token string
	err   error
}

func (source staticAccessTokenSource) FreshAccessToken(context.Context) (string, error) {
	return source.token, source.err
}

func newProxyTracerServer(t *testing.T, token, environmentID string, stream func(context.Context, *websocket.Conn)) *httptest.Server {
	t.Helper()
	return newProxyControlServer(t, token, environmentID, func(ctx context.Context, socket *websocket.Conn) {
		writeControlFrame(t, ctx, socket, connection.ControlFrame{Type: connection.ControlReady})
		stream(ctx, socket)
	})
}

func writeControlFrame(t *testing.T, ctx context.Context, socket *websocket.Conn, frame connection.ControlFrame) {
	t.Helper()
	content, err := json.Marshal(frame)
	if err != nil {
		t.Errorf("marshal control frame: %v", err)
		return
	}
	if err := socket.Write(ctx, websocket.MessageText, content); err != nil {
		t.Errorf("write control frame: %v", err)
	}
}

func newProxyControlServer(t *testing.T, token, environmentID string, stream func(context.Context, *websocket.Conn)) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/v1/environments/" + environmentID + "/connection-intents":
			key := request.Header.Get("Idempotency-Key")
			if request.Method != http.MethodPost || len(key) < 16 || len(key) > 40 {
				t.Errorf("intent request = method:%s key:%q", request.Method, key)
				http.Error(response, "bad request", http.StatusBadRequest)
				return
			}
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(contracts.ConnectionIntent{
				Id: "intent-01", EnvironmentId: environmentID, LogicalHostname: environmentID,
				ProxyUrl:  strings.Replace(server.URL, "https://", "wss://", 1) + "/v1/environments/" + environmentID + "/ssh",
				ExpiresAt: time.Now().Add(time.Minute),
			})
		case "/v1/environments/" + environmentID + "/ssh":
			connection, err := websocket.Accept(response, request, nil)
			if err != nil {
				t.Errorf("accept websocket: %v", err)
				return
			}
			defer connection.CloseNow()
			stream(request.Context(), connection)
		default:
			http.NotFound(response, request)
		}
	})
	server = httptest.NewTLSServer(handler)
	return server
}

func connectionIntentServer(t *testing.T, token string, intent contracts.ConnectionIntent) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(response).Encode(intent)
	}))
}
