// Command workflows serves the Restate workflow services over HTTP so a
// Restate instance can invoke them. Registering this deployment with that
// instance (`restate deployments register <url>`, or the Restate Cloud UI)
// is an operational step outside this binary.
//
// Only the BillingDelivery workflow is served today; see serviceDependencies
// for the production seams the remaining services are waiting on.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/billing"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/jackc/pgx/v5/pgxpool"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("workflows service stopped", "error", err)
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
		return fmt.Errorf("open database pool: %w", err)
	}
	defer pool.Close()
	store := db.NewStore(pool)
	polarClient, err := billing.NewPolarEventClient(
		config.polarEventsEndpoint, config.polarAccessToken, &http.Client{Timeout: 10 * time.Second},
	)
	if err != nil {
		return err
	}
	restateServer := server.NewRestate()
	for _, service := range buildServices(serviceDependencies{
		polarDeliverer: polarClient, polarStore: store, now: time.Now,
	}) {
		restateServer.Bind(service)
	}
	if err := restateServer.Start(ctx, config.listenAddress); err != nil {
		return fmt.Errorf("serve workflows: %w", err)
	}
	return nil
}

// serviceDependencies carries the production implementations behind each
// workflow service. Only BillingDelivery has a complete production dependency
// set today; each pending service joins by populating its field with a
// definition built from production implementations — buildServices already
// binds any non-nil field.
type serviceDependencies struct {
	polarDeliverer workflows.PolarEventDeliverer
	polarStore     workflows.PolarDeliveryStore
	now            func() time.Time

	// environmentCreate is pending a production
	// workflows.PinnedProfileVersionResolver (test fakes only today).
	environmentCreate restate.ServiceDefinition
	// profileResolve is pending a production workflows.CapsuleResolver.
	// db.Store now implements LoadProfileResolveState (slice S1b).
	profileResolve restate.ServiceDefinition
	// autoStop is pending production workflows.AutoStopSnapshotSource and
	// workflows.RuntimeStopDispatcher implementations.
	autoStop restate.ServiceDefinition
}

func buildServices(dependencies serviceDependencies) []restate.ServiceDefinition {
	services := []restate.ServiceDefinition{
		workflows.BillingDeliveryDefinition(dependencies.polarDeliverer, dependencies.polarStore, dependencies.now),
	}
	for _, pending := range []restate.ServiceDefinition{
		dependencies.environmentCreate, dependencies.profileResolve, dependencies.autoStop,
	} {
		if pending != nil {
			services = append(services, pending)
		}
	}
	return services
}

type config struct {
	databaseURL         string
	polarEventsEndpoint string
	polarAccessToken    string
	listenAddress       string
}

func loadConfig() (config, error) {
	config := config{
		databaseURL:         os.Getenv("DATABASE_URL"),
		polarEventsEndpoint: os.Getenv("POLAR_EVENTS_ENDPOINT"),
		polarAccessToken:    os.Getenv("POLAR_ACCESS_TOKEN"),
		listenAddress:       valueOrDefault("LISTEN_ADDR", ":9080"),
	}
	if config.databaseURL == "" || config.polarEventsEndpoint == "" || config.polarAccessToken == "" {
		return config, errors.New("DATABASE_URL, POLAR_EVENTS_ENDPOINT, and POLAR_ACCESS_TOKEN are required")
	}
	return config, nil
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
