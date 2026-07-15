package db

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

type permanentRepositoryError struct{ error }

func (permanentRepositoryError) Transient() bool { return false }

func (err permanentRepositoryError) Unwrap() error { return err.error }

func permanent(err error) error {
	if err == nil {
		return nil
	}
	var classified interface{ Transient() bool }
	if errors.As(err, &classified) {
		return err
	}
	return permanentRepositoryError{error: err}
}

func classifyRepositoryError(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && strings.HasPrefix(postgresError.Code, "23") {
		return permanent(err)
	}
	return err
}
