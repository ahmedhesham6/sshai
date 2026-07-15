package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/testfixtures"
)

func openTestDatabase(t *testing.T, ctx context.Context) (*sql.DB, string) {
	t.Helper()
	return testfixtures.OpenPostgres(t, ctx)
}
