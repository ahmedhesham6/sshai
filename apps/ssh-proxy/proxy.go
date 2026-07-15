// Package sshproxy authenticates and authorizes opaque SSH byte streams before
// bridging them from a regional WebSocket endpoint to a private Runtime.
package sshproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/coder/websocket"
)

const (
	defaultDialTimeout = 10 * time.Second
	defaultIdleTimeout = 2 * time.Minute
	defaultBufferBytes = 32 * 1024
)

var (
	ErrEnvironmentNotFound = errors.New("Environment SSH route not found")
	ErrRuntimeNotReady     = errors.New("Runtime SSH route not ready")
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

type ContextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type Config struct {
	Verifier      BearerVerifier
	Routes        EnvironmentRouter
	Dialer        ContextDialer
	StreamContext context.Context
	DialTimeout   time.Duration
	IdleTimeout   time.Duration
	BufferBytes   int
}

type proxy struct {
	verifier    BearerVerifier
	routes      EnvironmentRouter
	dialer      ContextDialer
	stream      context.Context
	dialTimeout time.Duration
	idleTimeout time.Duration
	bufferBytes int
}

func NewHandler(config Config) (http.Handler, error) {
	if config.Verifier == nil || config.Routes == nil || config.Dialer == nil {
		return nil, errors.New("create SSH proxy: verifier, Environment router, and dialer are required")
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = defaultDialTimeout
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = defaultIdleTimeout
	}
	if config.BufferBytes == 0 {
		config.BufferBytes = defaultBufferBytes
	}
	if config.StreamContext == nil {
		config.StreamContext = context.Background()
	}
	if config.DialTimeout < 0 || config.IdleTimeout < 0 || config.BufferBytes < 1 {
		return nil, errors.New("create SSH proxy: timeouts and buffer size must be positive")
	}
	handler := &proxy{
		verifier: config.Verifier, routes: config.Routes, dialer: config.Dialer,
		stream:      config.StreamContext,
		dialTimeout: config.DialTimeout, idleTimeout: config.IdleTimeout, bufferBytes: config.BufferBytes,
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
	route, err := proxy.routes.ResolveSSH(request.Context(), subject, request.PathValue("environment_id"))
	if errors.Is(err, ErrEnvironmentNotFound) {
		http.Error(response, "Environment not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, ErrRuntimeNotReady) {
		http.Error(response, "Runtime not ready", http.StatusConflict)
		return
	}
	if err != nil || !privateSSHAddress(route.PrivateAddress) {
		http.Error(response, "Environment SSH route unavailable", http.StatusServiceUnavailable)
		return
	}
	dialContext, cancelDial := context.WithTimeout(request.Context(), proxy.dialTimeout)
	runtime, err := proxy.dialer.DialContext(dialContext, "tcp", route.PrivateAddress)
	cancelDial()
	if err != nil {
		http.Error(response, "Runtime SSH unavailable", http.StatusBadGateway)
		return
	}
	defer runtime.Close()

	connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	streamContext, cancelStream := context.WithCancel(proxy.stream)
	defer cancelStream()
	web := websocket.NetConn(streamContext, connection, websocket.MessageBinary)
	defer web.Close()
	bridge(streamContext, web, runtime, proxy.idleTimeout, proxy.bufferBytes)
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
