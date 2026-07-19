package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	sshproxy "github.com/ahmedhesham6/sshai/apps/ssh-proxy"
	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("SSH proxy stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, config.databaseURL)
	if err != nil {
		return fmt.Errorf("open route database pool: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect route database: %w", err)
	}
	verifier, err := auth.NewVerifier(ctx, auth.VerifierConfig{
		JWKSURL: config.workOSJWKSURL, Issuer: config.workOSIssuer, ClientID: config.workOSClientID,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		return err
	}
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = verifier.Close(closeContext)
	}()

	routes := postgresRouteStore{queries: pool, region: config.region}
	starter, err := sshproxy.NewControlPlaneRuntimeStarter(
		config.controlPlaneURL,
		&http.Client{Timeout: 15 * time.Second},
		routes,
	)
	if err != nil {
		return err
	}
	handler, err := buildHandler(ctx, config, verifier, routes, starter, &net.Dialer{})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: config.listenAddress, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve SSH proxy: %w", err)
	}
	return nil
}

func buildHandler(ctx context.Context, config config, verifier sshproxy.BearerVerifier, routes sshproxy.EnvironmentRouter, starter sshproxy.RuntimeStarter, dialer sshproxy.ContextDialer) (http.Handler, error) {
	return sshproxy.NewHandler(sshproxy.Config{
		Verifier: verifier, Routes: routes, Starter: starter, Dialer: dialer,
		StreamContext: ctx,
		DialTimeout:   config.dialTimeout, IdleTimeout: config.idleTimeout,
		StartTimeout: config.startTimeout, PollInterval: config.pollInterval,
		ControlTimeout: config.controlTimeout, BufferBytes: config.bufferBytes,
	})
}

type config struct {
	databaseURL     string
	controlPlaneURL string
	region          string
	listenAddress   string
	workOSClientID  string
	workOSIssuer    string
	workOSJWKSURL   string
	dialTimeout     time.Duration
	idleTimeout     time.Duration
	startTimeout    time.Duration
	pollInterval    time.Duration
	controlTimeout  time.Duration
	bufferBytes     int
}

func loadConfig() (config, error) {
	clientID := os.Getenv("WORKOS_CLIENT_ID")
	result := config{
		databaseURL: os.Getenv("DATABASE_URL"), controlPlaneURL: os.Getenv("CONTROL_PLANE_URL"),
		region: os.Getenv("REGION"), listenAddress: valueOrDefault("LISTEN_ADDR", ":8082"),
		workOSClientID: clientID,
		workOSIssuer:   valueOrDefault("WORKOS_ISSUER", "https://api.workos.com/"),
		workOSJWKSURL:  valueOrDefault("WORKOS_JWKS_URL", "https://api.workos.com/sso/jwks/"+clientID),
	}
	if result.databaseURL == "" || result.controlPlaneURL == "" || result.region == "" || result.workOSClientID == "" {
		return result, errors.New("DATABASE_URL, CONTROL_PLANE_URL, REGION, and WORKOS_CLIENT_ID are required")
	}
	var err error
	if result.dialTimeout, err = durationOrDefault("DIAL_TIMEOUT", 10*time.Second); err != nil {
		return result, err
	}
	if result.idleTimeout, err = durationOrDefault("IDLE_TIMEOUT", 2*time.Minute); err != nil {
		return result, err
	}
	if result.startTimeout, err = durationOrDefault("START_WAIT_TIMEOUT", 10*time.Minute); err != nil {
		return result, err
	}
	if result.pollInterval, err = durationOrDefault("ROUTE_POLL_INTERVAL", 2*time.Second); err != nil {
		return result, err
	}
	if result.controlTimeout, err = durationOrDefault("CONTROL_WRITE_TIMEOUT", 10*time.Second); err != nil {
		return result, err
	}
	if result.bufferBytes, err = intOrDefault("BRIDGE_BUFFER_BYTES", 32*1024); err != nil {
		return result, err
	}
	return result, nil
}

func durationOrDefault(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func intOrDefault(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
