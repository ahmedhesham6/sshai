package testfixtures

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const PostgresImage = "postgres:18.4-alpine3.23@sha256:996d0920e4ff9df1fc19dacb904492f3c1ec0ec1cc338f0ad7123be7731c5f5e"

func OpenPostgres(t *testing.T, ctx context.Context) (*sql.DB, string) {
	t.Helper()
	container, err := postgres.Run(
		ctx,
		PostgresImage,
		postgres.WithDatabase("sshai"),
		postgres.WithUsername("sshai"),
		postgres.WithPassword("sshai"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL: %v", err)
	}
	testcontainers.CleanupContainer(t, container)
	connectionString, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get PostgreSQL connection string: %v", err)
	}
	database, err := sql.Open("pgx", connectionString)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close PostgreSQL: %v", err)
		}
	})
	return database, connectionString
}
