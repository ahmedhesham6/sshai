package sshproxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sshproxy "github.com/ahmedhesham6/sshai/apps/ssh-proxy"
	"github.com/ahmedhesham6/sshai/libs/auth"
	connectionprotocol "github.com/ahmedhesham6/sshai/libs/connection"
	"github.com/coder/websocket"
)

func TestProxyBridgesAuthenticatedBinaryWebSocketToOwnedReadyRuntime(t *testing.T) {
	echoAddress := startTCPEcho(t)
	dialer := &mappingDialer{target: echoAddress}
	handler, err := sshproxy.NewHandler(sshproxy.Config{
		Verifier: verifierFunc(func(_ context.Context, token string) (auth.Subject, error) {
			if token != "valid-token" {
				t.Fatalf("verified token = %q", token)
			}
			return auth.Subject{WorkOSUserID: "user-1"}, nil
		}),
		Routes: routeFunc(func(_ context.Context, owner auth.Subject, environmentID string) (sshproxy.EnvironmentSSHRoute, error) {
			if owner.WorkOSUserID != "user-1" || environmentID != "env-1" {
				t.Fatalf("route authorization = owner:%q environment:%q", owner.WorkOSUserID, environmentID)
			}
			return sshproxy.EnvironmentSSHRoute{PrivateAddress: "10.0.7.12:22"}, nil
		}),
		Starter: starterFunc(func(context.Context, string, string) (string, error) {
			t.Fatal("ready Runtime invoked starter")
			return "", errors.New("must not start")
		}),
		Dialer: dialer,
	})
	if err != nil {
		t.Fatalf("create SSH proxy: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid-token"}},
	})
	if err != nil {
		t.Fatalf("open SSH WebSocket: %v; response: %#v", err, response)
	}
	expectControl(t, ctx, connection, connectionprotocol.ControlReady)
	stream := websocket.NetConn(ctx, connection, websocket.MessageBinary)
	defer stream.Close()
	if err := stream.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := []byte("SSH-2.0-test\r\n\x00\xffopaque")
	if _, err := stream.Write(payload); err != nil {
		t.Fatalf("write SSH bytes: %v", err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, received); err != nil {
		t.Fatalf("read SSH bytes: %v", err)
	}
	if string(received) != string(payload) {
		t.Fatalf("bridged bytes = %q, want %q", received, payload)
	}
	if dialer.address != "10.0.7.12:22" {
		t.Fatalf("dialed Runtime = %q", dialer.address)
	}
}

func TestProxyStartsUnreadyRuntimeStreamsProgressThenBridgesBytes(t *testing.T) {
	const bearer = "BEARER_SECRET"
	const payloadSecret = "SSH_PAYLOAD_SECRET"
	echoAddress := startTCPEcho(t)
	dialer := &mappingDialer{target: echoAddress}
	routeCalls, startCalls := 0, 0
	handler, err := sshproxy.NewHandler(sshproxy.Config{
		Verifier: verifierFunc(func(_ context.Context, token string) (auth.Subject, error) {
			if token != bearer {
				t.Fatalf("verified bearer = %q", token)
			}
			return auth.Subject{WorkOSUserID: "user-1"}, nil
		}),
		Routes: routeFunc(func(_ context.Context, _ auth.Subject, _ string) (sshproxy.EnvironmentSSHRoute, error) {
			routeCalls++
			if routeCalls < 3 {
				return sshproxy.EnvironmentSSHRoute{}, sshproxy.ErrRuntimeNotReady
			}
			return sshproxy.EnvironmentSSHRoute{PrivateAddress: "10.0.7.12:22"}, nil
		}),
		Starter: starterFunc(func(_ context.Context, token, environmentID string) (string, error) {
			startCalls++
			if token != bearer || environmentID != "env-1" {
				t.Fatalf("start request = bearer:%q Environment:%q", token, environmentID)
			}
			return "operation-1", nil
		}),
		Dialer: dialer, PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + bearer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	steps := map[string]bool{}
	var encodedFrames bytes.Buffer
	for {
		frame := expectControl(t, ctx, socket, "")
		_ = json.NewEncoder(&encodedFrames).Encode(frame)
		if frame.Step != "" {
			steps[frame.Step] = true
		}
		if frame.Type == connectionprotocol.ControlReady {
			if frame.OperationID != "operation-1" {
				t.Fatalf("ready Operation = %q", frame.OperationID)
			}
			break
		}
		if frame.Type != connectionprotocol.ControlProgress {
			t.Fatalf("pre-ready frame = %#v", frame)
		}
	}
	for _, step := range []string{"starting-runtime", "waiting-for-guest", "resolving-route"} {
		if !steps[step] {
			t.Fatalf("progress steps = %v, missing %q", steps, step)
		}
	}
	if startCalls != 1 {
		t.Fatalf("Runtime starter calls = %d, want 1", startCalls)
	}
	if strings.Contains(encodedFrames.String(), bearer) || strings.Contains(encodedFrames.String(), payloadSecret) {
		t.Fatalf("control frames exposed bearer or SSH payload: %s", encodedFrames.String())
	}
	stream := websocket.NetConn(ctx, socket, websocket.MessageBinary)
	if _, err := stream.Write([]byte(payloadSecret)); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payloadSecret))
	if _, err := io.ReadFull(stream, received); err != nil || string(received) != payloadSecret {
		t.Fatalf("bridged payload = %q, %v", received, err)
	}
}

func TestProxyCreditBlockedSendsFailedStartStepWithoutDial(t *testing.T) {
	dialed := 0
	config := testProxyConfig(dialerFunc(func(context.Context, string, string) (net.Conn, error) {
		dialed++
		return nil, errors.New("must not dial")
	}))
	config.Routes = routeFunc(func(context.Context, auth.Subject, string) (sshproxy.EnvironmentSSHRoute, error) {
		return sshproxy.EnvironmentSSHRoute{}, sshproxy.ErrRuntimeNotReady
	})
	config.Starter = starterFunc(func(context.Context, string, string) (string, error) {
		return "", sshproxy.ErrCreditsPolicyBlocked
	})
	handler, err := sshproxy.NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	expectControl(t, ctx, socket, connectionprotocol.ControlProgress)
	failed := expectControl(t, ctx, socket, connectionprotocol.ControlFailed)
	if failed.Step != "starting-runtime" || !strings.Contains(failed.Message, "credit policy") || dialed != 0 {
		t.Fatalf("credit failure = %#v, dials:%d", failed, dialed)
	}
}

func TestProxyReadinessDeadlineSendsFailedWaitingStep(t *testing.T) {
	config := testProxyConfig(dialerFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("must not dial")
	}))
	config.Routes = routeFunc(func(context.Context, auth.Subject, string) (sshproxy.EnvironmentSSHRoute, error) {
		return sshproxy.EnvironmentSSHRoute{}, sshproxy.ErrRuntimeNotReady
	})
	config.Starter = starterFunc(func(context.Context, string, string) (string, error) {
		return "operation-timeout", nil
	})
	config.StartTimeout = 35 * time.Millisecond
	config.PollInterval = 5 * time.Millisecond
	handler, err := sshproxy.NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	for {
		frame := expectControl(t, ctx, socket, "")
		if frame.Type == connectionprotocol.ControlFailed {
			if frame.Step != "waiting-for-guest" || frame.OperationID != "operation-timeout" {
				t.Fatalf("deadline failure = %#v", frame)
			}
			break
		}
	}
}

func TestProxyRejectsClientTextFrameWhileWaitingForStart(t *testing.T) {
	started := make(chan struct{})
	config := testProxyConfig(dialerFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("must not dial")
	}))
	config.Routes = routeFunc(func(context.Context, auth.Subject, string) (sshproxy.EnvironmentSSHRoute, error) {
		return sshproxy.EnvironmentSSHRoute{}, sshproxy.ErrRuntimeNotReady
	})
	config.Starter = starterFunc(func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", context.Cause(ctx)
	})
	handler, err := sshproxy.NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	expectControl(t, ctx, socket, connectionprotocol.ControlProgress)
	<-started
	if err := socket.Write(ctx, websocket.MessageText, []byte("not SSH bytes")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := socket.Read(ctx); err == nil {
		t.Fatal("pre-ready text frame did not close the WebSocket")
	}
}

func TestProxyRejectsUnauthorizedEnvironmentBeforeUpgradeOrStart(t *testing.T) {
	for _, test := range []struct {
		name       string
		token      string
		verifyErr  error
		routeErr   error
		wantStatus int
	}{
		{name: "missing bearer", wantStatus: http.StatusUnauthorized},
		{name: "invalid bearer", token: "invalid", verifyErr: errors.New("invalid token"), wantStatus: http.StatusUnauthorized},
		{name: "foreign Environment", token: "valid", routeErr: sshproxy.ErrEnvironmentNotFound, wantStatus: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			verified, routed, started, dialed := 0, 0, 0, 0
			handler, err := sshproxy.NewHandler(sshproxy.Config{
				Verifier: verifierFunc(func(_ context.Context, _ string) (auth.Subject, error) {
					verified++
					return auth.Subject{WorkOSUserID: "user-1"}, test.verifyErr
				}),
				Routes: routeFunc(func(_ context.Context, _ auth.Subject, _ string) (sshproxy.EnvironmentSSHRoute, error) {
					routed++
					return sshproxy.EnvironmentSSHRoute{PrivateAddress: "10.0.7.12:22"}, test.routeErr
				}),
				Starter: starterFunc(func(context.Context, string, string) (string, error) {
					started++
					return "", errors.New("must not start")
				}),
				Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
					dialed++
					return nil, errors.New("must not dial")
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			header := http.Header{}
			if test.token != "" {
				header.Set("Authorization", "Bearer "+test.token)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			connection, response, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{HTTPHeader: header})
			if connection != nil {
				connection.CloseNow()
			}
			if err == nil || response == nil || response.StatusCode != test.wantStatus {
				t.Fatalf("rejection = error:%v response:%#v, want HTTP %d", err, response, test.wantStatus)
			}
			if dialed != 0 {
				t.Fatalf("private Runtime dialed %d times", dialed)
			}
			if started != 0 {
				t.Fatalf("Runtime start requested %d times", started)
			}
			if test.token == "" && (verified != 0 || routed != 0) {
				t.Fatalf("missing bearer reached verifier:%d router:%d", verified, routed)
			}
			if test.verifyErr != nil && routed != 0 {
				t.Fatalf("invalid bearer reached router %d times", routed)
			}
		})
	}
}

func TestProxyHalfClosesRuntimeWhenWebSocketInputEnds(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	runtimeEOF := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(io.Discard, connection)
		close(runtimeEOF)
	}()
	handler := newTestProxy(t, &mappingDialer{target: listener.Addr().String()})
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	expectControl(t, ctx, connection, connectionprotocol.ControlReady)
	if err := connection.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close WebSocket input: %v", err)
	}
	select {
	case <-runtimeEOF:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Runtime did not observe a half-close after WebSocket input ended")
	}
}

func TestProxyDisconnectCancelsAndClosesThePrivateRuntimeConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	runtimeClosed := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(io.Discard, connection)
		close(runtimeClosed)
	}()
	handler := newTestProxy(t, &mappingDialer{target: listener.Addr().String()})
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	expectControl(t, ctx, connection, connectionprotocol.ControlReady)
	if err := connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtimeClosed:
	case <-time.After(time.Second):
		t.Fatal("private Runtime connection survived WebSocket disconnect")
	}
}

func TestProxyStreamContextCancellationClosesThePrivateRuntimeConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	runtimeClosed := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(io.Discard, connection)
		close(runtimeClosed)
	}()
	streamContext, cancelStreams := context.WithCancel(context.Background())
	config := testProxyConfig(&mappingDialer{target: listener.Addr().String()})
	config.StreamContext = streamContext
	handler, err := sshproxy.NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	expectControl(t, ctx, connection, connectionprotocol.ControlReady)
	cancelStreams()
	select {
	case <-runtimeClosed:
	case <-time.After(time.Second):
		t.Fatal("private Runtime connection survived proxy stream cancellation")
	}
}

func TestProxyRejectsNonPrivateOrNonSSHRoutesBeforeDial(t *testing.T) {
	for _, address := range []string{"127.0.0.1:22", "8.8.8.8:22", "runtime.internal:22", "10.0.7.12:2222"} {
		t.Run(address, func(t *testing.T) {
			dialed := false
			handler, err := sshproxy.NewHandler(sshproxy.Config{
				Verifier: verifierFunc(func(context.Context, string) (auth.Subject, error) {
					return auth.Subject{WorkOSUserID: "user-1"}, nil
				}),
				Routes: routeFunc(func(context.Context, auth.Subject, string) (sshproxy.EnvironmentSSHRoute, error) {
					return sshproxy.EnvironmentSSHRoute{PrivateAddress: address}, nil
				}),
				Starter: starterFunc(func(context.Context, string, string) (string, error) {
					return "", errors.New("must not start")
				}),
				Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
					dialed = true
					return nil, errors.New("must not dial")
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			connection, response, dialErr := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
				HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
			})
			if connection != nil {
				connection.CloseNow()
			}
			if dialErr == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("route rejection = error:%v response:%#v", dialErr, response)
			}
			if dialed {
				t.Fatal("unsafe Runtime route was dialed")
			}
		})
	}
}

func TestProxyStreamsConcurrentSSHConnectionsRaceSafely(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	handler := newTestProxy(t, dialerFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	}))
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 8)
	for index := 0; index < cap(results); index++ {
		go func() {
			connection, _, dialErr := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
				HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
			})
			if dialErr != nil {
				results <- dialErr
				return
			}
			frame, readErr := readControl(ctx, connection)
			if readErr != nil || frame.Type != connectionprotocol.ControlReady {
				results <- fmt.Errorf("ready control = %#v, %v", frame, readErr)
				return
			}
			stream := websocket.NetConn(ctx, connection, websocket.MessageBinary)
			defer stream.Close()
			payload := []byte("SSH concurrent opaque bytes")
			if _, writeErr := stream.Write(payload); writeErr != nil {
				results <- writeErr
				return
			}
			reply := make([]byte, len(payload))
			if _, readErr := io.ReadFull(stream, reply); readErr != nil {
				results <- readErr
				return
			}
			if !bytes.Equal(reply, payload) {
				results <- errors.New("SSH payload changed in transit")
				return
			}
			results <- nil
		}()
	}
	for index := 0; index < cap(results); index++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent SSH stream: %v", err)
		}
	}
}

func TestProxyIdleDeadlineStaysOpenDuringOneWaySSHActivity(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		for value := byte(0); value < 10; value++ {
			if _, writeErr := connection.Write([]byte{value}); writeErr != nil {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()
	config := testProxyConfig(&mappingDialer{target: listener.Addr().String()})
	config.IdleTimeout = 40 * time.Millisecond
	handler, err := sshproxy.NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	expectControl(t, ctx, connection, connectionprotocol.ControlReady)
	stream := websocket.NetConn(ctx, connection, websocket.MessageBinary)
	defer stream.Close()
	received := make([]byte, 10)
	if _, err := io.ReadFull(stream, received); err != nil {
		t.Fatalf("read active one-way SSH stream: %v", err)
	}
	if !bytes.Equal(received, []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatalf("one-way SSH bytes = %v", received)
	}
}

func TestProxyKeepsWebSocketInputOpenAfterRuntimeHalfClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		tcp := connection.(*net.TCPConn)
		_, _ = tcp.Write([]byte("done"))
		_ = tcp.CloseWrite()
		input := make([]byte, len("late-input"))
		_, readErr := io.ReadFull(tcp, input)
		if readErr == nil {
			received <- string(input)
		}
	}()
	handler := newTestProxy(t, &mappingDialer{target: listener.Addr().String()})
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	expectControl(t, ctx, connection, connectionprotocol.ControlReady)
	stream := websocket.NetConn(ctx, connection, websocket.MessageBinary)
	defer stream.Close()
	output := make([]byte, len("done"))
	if _, err := io.ReadFull(stream, output); err != nil || string(output) != "done" {
		t.Fatalf("Runtime output = %q, %v", output, err)
	}
	if _, err := stream.Write([]byte("late-input")); err != nil {
		t.Fatalf("write after Runtime half-close: %v", err)
	}
	select {
	case input := <-received:
		if input != "late-input" {
			t.Fatalf("Runtime received %q", input)
		}
	case <-time.After(time.Second):
		t.Fatal("Runtime half-close terminated WebSocket input")
	}
}

func TestProxyRejectsTextFramesWithoutForwardingPayload(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan int64, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		count, _ := io.Copy(io.Discard, connection)
		received <- count
	}()
	handler := newTestProxy(t, &mappingDialer{target: listener.Addr().String()})
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, websocketURL(server.URL)+"/v1/environments/env-1/ssh", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer valid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	expectControl(t, ctx, connection, connectionprotocol.ControlReady)
	if err := connection.Write(ctx, websocket.MessageText, []byte("not SSH bytes")); err != nil {
		t.Fatal(err)
	}
	_, _, _ = connection.Read(ctx)
	_ = connection.CloseNow()
	select {
	case count := <-received:
		if count != 0 {
			t.Fatalf("Runtime received %d text-frame bytes", count)
		}
	case <-time.After(time.Second):
		t.Fatal("text-frame rejection did not clean up Runtime connection")
	}
}

func newTestProxy(t *testing.T, dialer sshproxy.ContextDialer) http.Handler {
	t.Helper()
	handler, err := sshproxy.NewHandler(testProxyConfig(dialer))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func testProxyConfig(dialer sshproxy.ContextDialer) sshproxy.Config {
	return sshproxy.Config{
		Verifier: verifierFunc(func(context.Context, string) (auth.Subject, error) {
			return auth.Subject{WorkOSUserID: "user-1"}, nil
		}),
		Routes: routeFunc(func(context.Context, auth.Subject, string) (sshproxy.EnvironmentSSHRoute, error) {
			return sshproxy.EnvironmentSSHRoute{PrivateAddress: "10.0.7.12:22"}, nil
		}),
		Starter: starterFunc(func(context.Context, string, string) (string, error) {
			return "", errors.New("ready Runtime must not start")
		}),
		Dialer: dialer,
	}
}

type verifierFunc func(context.Context, string) (auth.Subject, error)

func (verify verifierFunc) Verify(ctx context.Context, token string) (auth.Subject, error) {
	return verify(ctx, token)
}

type routeFunc func(context.Context, auth.Subject, string) (sshproxy.EnvironmentSSHRoute, error)

func (route routeFunc) ResolveSSH(ctx context.Context, owner auth.Subject, environmentID string) (sshproxy.EnvironmentSSHRoute, error) {
	return route(ctx, owner, environmentID)
}

type starterFunc func(context.Context, string, string) (string, error)

func (start starterFunc) EnsureStarted(ctx context.Context, bearer, environmentID string) (string, error) {
	return start(ctx, bearer, environmentID)
}

type mappingDialer struct {
	target  string
	address string
}

type dialerFunc func(context.Context, string, string) (net.Conn, error)

func (dial dialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return dial(ctx, network, address)
}

func (dialer *mappingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	dialer.address = address
	return (&net.Dialer{}).DialContext(ctx, network, dialer.target)
}

func startTCPEcho(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	return listener.Addr().String()
}

func websocketURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func expectControl(t *testing.T, ctx context.Context, socket *websocket.Conn, want connectionprotocol.ControlFrameType) connectionprotocol.ControlFrame {
	t.Helper()
	frame, err := readControl(ctx, socket)
	if err != nil {
		t.Fatalf("read %s control frame: %v", want, err)
	}
	if want != "" && frame.Type != want {
		t.Fatalf("control frame = %#v, want type %q", frame, want)
	}
	return frame
}

func readControl(ctx context.Context, socket *websocket.Conn) (connectionprotocol.ControlFrame, error) {
	messageType, payload, err := socket.Read(ctx)
	if err != nil {
		return connectionprotocol.ControlFrame{}, err
	}
	if messageType != websocket.MessageText {
		return connectionprotocol.ControlFrame{}, fmt.Errorf("control message type = %v", messageType)
	}
	var frame connectionprotocol.ControlFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		return connectionprotocol.ControlFrame{}, err
	}
	return frame, nil
}
