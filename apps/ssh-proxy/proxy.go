// Package sshproxy authenticates and authorizes opaque SSH byte streams before
// bridging them from a regional WebSocket endpoint to a private Runtime.
package sshproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ahmedhesham6/sshai/libs/auth"
	connectionprotocol "github.com/ahmedhesham6/sshai/libs/connection"
	"github.com/coder/websocket"
)

const (
	defaultDialTimeout         = 10 * time.Second
	defaultIdleTimeout         = 2 * time.Minute
	defaultStartWaitTimeout    = 10 * time.Minute
	defaultRoutePollInterval   = 2 * time.Second
	defaultControlWriteTimeout = 10 * time.Second
	defaultBufferBytes         = 32 * 1024
	preBridgeReadLimit         = 1 << 20
)

var (
	ErrEnvironmentNotFound = errors.New("Environment SSH route not found")
	ErrRuntimeNotReady     = errors.New("Runtime SSH route not ready")
	ErrRuntimeStartFailed  = errors.New("Runtime start failed")
)

type BearerVerifier interface {
	Verify(context.Context, string) (auth.Subject, error)
}

// EnvironmentRouter authorizes owner access and returns only a current,
// ready Runtime route. Missing, foreign, and unready Environments return an
// error instead of an address.
type EnvironmentRouter interface {
	ResolveSSH(context.Context, auth.Subject, string) (EnvironmentSSHRoute, error)
}

type EnvironmentSSHRoute struct {
	PrivateAddress string
}

// RuntimeStarter asks the control plane to ensure that the Environment's
// current Runtime is starting. bearer is the already-verified user access
// token and must be forwarded without logging or including it in errors.
type RuntimeStarter interface {
	EnsureStarted(context.Context, string, string) (operationID string, err error)
}

type ContextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type Config struct {
	Verifier       BearerVerifier
	Routes         EnvironmentRouter
	Starter        RuntimeStarter
	Dialer         ContextDialer
	StreamContext  context.Context
	DialTimeout    time.Duration
	IdleTimeout    time.Duration
	StartTimeout   time.Duration
	PollInterval   time.Duration
	ControlTimeout time.Duration
	BufferBytes    int
}

type proxy struct {
	verifier       BearerVerifier
	routes         EnvironmentRouter
	starter        RuntimeStarter
	dialer         ContextDialer
	stream         context.Context
	dialTimeout    time.Duration
	idleTimeout    time.Duration
	startTimeout   time.Duration
	pollInterval   time.Duration
	controlTimeout time.Duration
	bufferBytes    int
}

func NewHandler(config Config) (http.Handler, error) {
	if config.Verifier == nil || config.Routes == nil || config.Starter == nil || config.Dialer == nil {
		return nil, errors.New("create SSH proxy: verifier, Environment router, Runtime starter, and dialer are required")
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = defaultDialTimeout
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = defaultIdleTimeout
	}
	if config.StartTimeout == 0 {
		config.StartTimeout = defaultStartWaitTimeout
	}
	if config.PollInterval == 0 {
		config.PollInterval = defaultRoutePollInterval
	}
	if config.ControlTimeout == 0 {
		config.ControlTimeout = defaultControlWriteTimeout
	}
	if config.BufferBytes == 0 {
		config.BufferBytes = defaultBufferBytes
	}
	if config.StreamContext == nil {
		config.StreamContext = context.Background()
	}
	if config.DialTimeout < 0 || config.IdleTimeout < 0 || config.StartTimeout < 0 ||
		config.PollInterval < 0 || config.ControlTimeout < 0 || config.BufferBytes < 1 {
		return nil, errors.New("create SSH proxy: timeouts and buffer size must be positive")
	}
	handler := &proxy{
		verifier: config.Verifier, routes: config.Routes, starter: config.Starter, dialer: config.Dialer,
		stream:      config.StreamContext,
		dialTimeout: config.DialTimeout, idleTimeout: config.IdleTimeout, startTimeout: config.StartTimeout,
		pollInterval: config.PollInterval, controlTimeout: config.ControlTimeout, bufferBytes: config.BufferBytes,
	}
	router := http.NewServeMux()
	router.HandleFunc("GET /v1/environments/{environment_id}/ssh", handler.serveSSH)
	return router, nil
}

func (proxy *proxy) serveSSH(response http.ResponseWriter, request *http.Request) {
	token, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		http.Error(response, "authentication required", http.StatusUnauthorized)
		return
	}
	subject, err := proxy.verifier.Verify(request.Context(), token)
	if err != nil || subject.WorkOSUserID == "" {
		http.Error(response, "authentication required", http.StatusUnauthorized)
		return
	}
	environmentID := request.PathValue("environment_id")
	route, err := proxy.routes.ResolveSSH(request.Context(), subject, environmentID)
	if errors.Is(err, ErrEnvironmentNotFound) {
		http.Error(response, "Environment not found", http.StatusNotFound)
		return
	}
	notReady := errors.Is(err, ErrRuntimeNotReady)
	if (!notReady && err != nil) || (!notReady && !privateSSHAddress(route.PrivateAddress)) {
		http.Error(response, "Environment SSH route unavailable", http.StatusServiceUnavailable)
		return
	}

	connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(preBridgeReadLimit)

	socketContext, cancelSocket := context.WithCancel(proxy.stream)
	stopRequestCancellation := context.AfterFunc(request.Context(), cancelSocket)
	waitContext, cancelWait := context.WithTimeout(socketContext, proxy.startTimeout)
	defer func() {
		stopRequestCancellation()
		cancelWait()
		cancelSocket()
	}()
	guard := watchPreBridgeClient(socketContext, connection)

	operationID := ""
	if notReady {
		if !proxy.writeControl(waitContext, connection, connectionprotocol.ControlFrame{
			Type: connectionprotocol.ControlProgress, Step: "starting-runtime", Message: "Requesting Runtime start.",
		}) {
			return
		}
		operationID, err = proxy.ensureStarted(waitContext, guard, token, environmentID)
		if err != nil {
			if errors.Is(err, errClientFrameBeforeReady) {
				return
			}
			proxy.failControl(connection, operationID, "starting-runtime", startFailureMessage(err))
			return
		}
		if !proxy.writeControl(waitContext, connection, connectionprotocol.ControlFrame{
			Type: connectionprotocol.ControlProgress, OperationID: operationID, Step: "waiting-for-guest", Message: "Waiting for Runtime readiness.",
		}) {
			return
		}
		route, err = proxy.waitForRoute(waitContext, guard, connection, subject, environmentID, operationID)
		if err != nil {
			if errors.Is(err, errClientFrameBeforeReady) {
				return
			}
			step, message := routeFailure(err)
			proxy.failControl(connection, operationID, step, message)
			return
		}
	}

	runtime, err := proxy.dialRuntime(waitContext, guard, route.PrivateAddress)
	if err != nil {
		if !errors.Is(err, errClientFrameBeforeReady) {
			proxy.failControl(connection, operationID, "dialing-runtime", "The Runtime SSH service is unavailable. Persistent Environment state is intact.")
		}
		return
	}
	defer runtime.Close()
	select {
	case event := <-guard.result:
		if event.err == nil {
			event.err = errClientFrameBeforeReady
		}
		return
	default:
	}
	if !proxy.writeControl(waitContext, connection, connectionprotocol.ControlFrame{
		Type: connectionprotocol.ControlReady, OperationID: operationID, Step: "ready", Message: "Runtime SSH is ready.",
	}) {
		return
	}

	streamContext, cancelStream := context.WithCancel(socketContext)
	defer cancelStream()
	web := &promotedWebSocketConn{
		Conn: websocket.NetConn(streamContext, connection, websocket.MessageBinary),
		ctx:  streamContext, first: guard.result,
	}
	defer web.Close()
	bridge(streamContext, web, runtime, proxy.idleTimeout, proxy.bufferBytes)
}

var errClientFrameBeforeReady = errors.New("client frame received before ready")

type preBridgeGuard struct {
	result <-chan clientFrame
}

type clientFrame struct {
	messageType websocket.MessageType
	payload     []byte
	err         error
}

func watchPreBridgeClient(ctx context.Context, socket *websocket.Conn) *preBridgeGuard {
	result := make(chan clientFrame, 1)
	go func() {
		messageType, payload, err := socket.Read(ctx)
		result <- clientFrame{messageType: messageType, payload: payload, err: err}
	}()
	return &preBridgeGuard{result: result}
}

func clientFrameError(event clientFrame) error {
	if event.err != nil {
		return event.err
	}
	return errClientFrameBeforeReady
}

type promotedWebSocketConn struct {
	net.Conn
	ctx     context.Context
	first   <-chan clientFrame
	pending []byte
}

func (connection *promotedWebSocketConn) Read(buffer []byte) (int, error) {
	if len(connection.pending) > 0 {
		read := copy(buffer, connection.pending)
		connection.pending = connection.pending[read:]
		return read, nil
	}
	if connection.first != nil {
		select {
		case <-connection.ctx.Done():
			return 0, context.Cause(connection.ctx)
		case event := <-connection.first:
			connection.first = nil
			if event.err != nil {
				return 0, event.err
			}
			if event.messageType != websocket.MessageBinary {
				return 0, errors.New("received non-binary WebSocket message")
			}
			connection.pending = event.payload
		}
		if len(connection.pending) > 0 {
			read := copy(buffer, connection.pending)
			connection.pending = connection.pending[read:]
			return read, nil
		}
	}
	return connection.Conn.Read(buffer)
}

func (proxy *proxy) ensureStarted(ctx context.Context, guard *preBridgeGuard, bearer, environmentID string) (string, error) {
	type result struct {
		operationID string
		err         error
	}
	completed := make(chan result, 1)
	go func() {
		operationID, err := proxy.starter.EnsureStarted(ctx, bearer, environmentID)
		completed <- result{operationID: operationID, err: err}
	}()
	select {
	case event := <-guard.result:
		return "", clientFrameError(event)
	case <-ctx.Done():
		return "", context.Cause(ctx)
	case result := <-completed:
		return result.operationID, result.err
	}
}

func (proxy *proxy) waitForRoute(ctx context.Context, guard *preBridgeGuard, socket *websocket.Conn, subject auth.Subject, environmentID, operationID string) (EnvironmentSSHRoute, error) {
	for {
		if !proxy.writeControl(ctx, socket, connectionprotocol.ControlFrame{
			Type: connectionprotocol.ControlProgress, OperationID: operationID, Step: "resolving-route", Message: "Checking current-boot SSH readiness.",
		}) {
			return EnvironmentSSHRoute{}, context.Cause(ctx)
		}
		route, err := proxy.resolveRoute(ctx, guard, subject, environmentID)
		if err == nil {
			if !privateSSHAddress(route.PrivateAddress) {
				return EnvironmentSSHRoute{}, errors.New("unsafe Runtime route")
			}
			return route, nil
		}
		if !errors.Is(err, ErrRuntimeNotReady) {
			return EnvironmentSSHRoute{}, err
		}
		if !proxy.writeControl(ctx, socket, connectionprotocol.ControlFrame{
			Type: connectionprotocol.ControlProgress, OperationID: operationID, Step: "waiting-for-guest", Message: "Runtime is not ready yet.",
		}) {
			return EnvironmentSSHRoute{}, context.Cause(ctx)
		}
		timer := time.NewTimer(proxy.pollInterval)
		select {
		case event := <-guard.result:
			stopTimer(timer)
			return EnvironmentSSHRoute{}, clientFrameError(event)
		case <-ctx.Done():
			stopTimer(timer)
			return EnvironmentSSHRoute{}, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

func (proxy *proxy) resolveRoute(ctx context.Context, guard *preBridgeGuard, subject auth.Subject, environmentID string) (EnvironmentSSHRoute, error) {
	type result struct {
		route EnvironmentSSHRoute
		err   error
	}
	completed := make(chan result, 1)
	go func() {
		route, err := proxy.routes.ResolveSSH(ctx, subject, environmentID)
		completed <- result{route: route, err: err}
	}()
	select {
	case event := <-guard.result:
		return EnvironmentSSHRoute{}, clientFrameError(event)
	case <-ctx.Done():
		return EnvironmentSSHRoute{}, context.Cause(ctx)
	case result := <-completed:
		return result.route, result.err
	}
}

type dialResult struct {
	connection net.Conn
	err        error
}

func (proxy *proxy) dialRuntime(ctx context.Context, guard *preBridgeGuard, address string) (net.Conn, error) {
	dialContext, cancelDial := context.WithTimeout(ctx, proxy.dialTimeout)
	defer cancelDial()
	completed := make(chan dialResult, 1)
	go func() {
		connection, err := proxy.dialer.DialContext(dialContext, "tcp", address)
		completed <- dialResult{connection: connection, err: err}
	}()
	select {
	case event := <-guard.result:
		cancelDial()
		go closeDialResult(completed)
		return nil, clientFrameError(event)
	case <-dialContext.Done():
		go closeDialResult(completed)
		return nil, context.Cause(dialContext)
	case result := <-completed:
		return result.connection, result.err
	}
}

func closeDialResult(completed <-chan dialResult) {
	result := <-completed
	if result.connection != nil {
		_ = result.connection.Close()
	}
}

func (proxy *proxy) writeControl(ctx context.Context, socket *websocket.Conn, frame connectionprotocol.ControlFrame) bool {
	payload, err := json.Marshal(frame)
	if err != nil {
		return false
	}
	writeContext, cancel := context.WithTimeout(ctx, proxy.controlTimeout)
	defer cancel()
	return socket.Write(writeContext, websocket.MessageText, payload) == nil
}

func (proxy *proxy) failControl(socket *websocket.Conn, operationID, step, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), proxy.controlTimeout)
	defer cancel()
	_ = proxy.writeControl(ctx, socket, connectionprotocol.ControlFrame{
		Type: connectionprotocol.ControlFailed, OperationID: operationID, Step: step, Message: message,
	})
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func startFailureMessage(err error) string {
	switch {
	case errors.Is(err, ErrCreditsPolicyBlocked):
		return "Runtime start is blocked by the credit policy. Persistent Environment state is intact."
	case errors.Is(err, ErrStartAuthorization):
		return "Runtime start authorization failed. Persistent Environment state is intact."
	default:
		return "The Runtime could not be started. Persistent Environment state is intact."
	}
}

func routeFailure(err error) (string, string) {
	switch {
	case errors.Is(err, errClientFrameBeforeReady):
		return "client-protocol", "Client frames are not accepted before Runtime readiness."
	case errors.Is(err, context.DeadlineExceeded):
		return "waiting-for-guest", "Runtime readiness timed out. Persistent Environment state is intact."
	case errors.Is(err, ErrRuntimeStartFailed):
		return "waiting-for-guest", "Runtime start failed. Persistent Environment state is intact."
	default:
		return "resolving-route", "The current-boot SSH route could not be resolved. Persistent Environment state is intact."
	}
}

func bearerToken(authorization string) (string, bool) {
	scheme, token, found := strings.Cut(strings.TrimSpace(authorization), " ")
	return token, found && strings.EqualFold(scheme, "Bearer") && token != ""
}

func privateSSHAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "22" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsPrivate()
}

type deadlineConn struct {
	net.Conn
	deadlines *streamDeadlines
}

func (connection deadlineConn) Read(buffer []byte) (int, error) {
	if err := connection.deadlines.touch(); err != nil {
		return 0, err
	}
	read, err := connection.Conn.Read(buffer)
	if read > 0 {
		_ = connection.deadlines.touch()
	}
	return read, err
}

func (connection deadlineConn) Write(buffer []byte) (int, error) {
	if err := connection.deadlines.touch(); err != nil {
		return 0, err
	}
	written, err := connection.Conn.Write(buffer)
	if written > 0 {
		_ = connection.deadlines.touch()
	}
	return written, err
}

type streamDeadlines struct {
	mu          sync.Mutex
	timeout     time.Duration
	connections []net.Conn
}

func (deadlines *streamDeadlines) touch() error {
	deadlines.mu.Lock()
	defer deadlines.mu.Unlock()
	deadline := time.Now().Add(deadlines.timeout)
	for _, connection := range deadlines.connections {
		if err := connection.SetDeadline(deadline); err != nil {
			return err
		}
	}
	return nil
}

func bridge(ctx context.Context, web, runtime net.Conn, idleTimeout time.Duration, bufferBytes int) {
	deadlines := &streamDeadlines{timeout: idleTimeout, connections: []net.Conn{web, runtime}}
	web = deadlineConn{Conn: web, deadlines: deadlines}
	runtime = deadlineConn{Conn: runtime, deadlines: deadlines}
	results := make(chan error, 2)
	copyDirection := func(destination, source net.Conn) {
		_, err := io.CopyBuffer(destination, source, make([]byte, bufferBytes))
		closeWrite(destination)
		results <- err
	}
	go copyDirection(runtime, web)
	go copyDirection(web, runtime)
	first := <-results
	if first != nil || ctx.Err() != nil {
		_ = web.SetDeadline(time.Now())
		_ = runtime.SetDeadline(time.Now())
	}
	<-results
}

func closeWrite(connection net.Conn) {
	if bounded, ok := connection.(deadlineConn); ok {
		connection = bounded.Conn
	}
	if half, ok := connection.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
	}
}
