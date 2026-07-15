package db

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPermanentRepositoryErrorPreservesCauseAndClassification(t *testing.T) {
	cause := errors.New("ownership conflict")
	err := permanent(fmt.Errorf("inventory Environment State: %w", cause))
	if !errors.Is(err, cause) {
		t.Fatalf("permanent repository error does not preserve cause: %v", err)
	}
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || classified.Transient() {
		t.Fatalf("permanent repository error classification = %T %v", err, err)
	}
	if markedAgain := permanent(err); markedAgain != err {
		t.Fatalf("re-marked permanent repository error = %T %v", markedAgain, markedAgain)
	}
}

func TestRepositoryErrorClassificationOnlyTerminatesIntegrityViolations(t *testing.T) {
	integrity := &pgconn.PgError{Code: "23505", Message: "duplicate identity"}
	if err := classifyRepositoryError(integrity); err == integrity {
		t.Fatal("integrity violation remained unclassified")
	} else {
		var classified interface{ Transient() bool }
		if !errors.As(err, &classified) || classified.Transient() {
			t.Fatalf("integrity violation classification = %T %v", err, err)
		}
	}
	for _, code := range []string{"08006", "40001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			transient := &pgconn.PgError{Code: code, Message: "transient database failure"}
			if err := classifyRepositoryError(transient); err != transient {
				t.Fatalf("transient PostgreSQL error classification = %T %v", err, err)
			}
		})
	}
}
