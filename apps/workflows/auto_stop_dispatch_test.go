package workflows

import (
	"errors"
	"testing"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	restate "github.com/restatedev/sdk-go"
)

func TestClassifyAutoStopDispatchError(t *testing.T) {
	permanent := autoStopDispatchTestError{err: errors.New("permanent"), transient: false}
	tests := []struct {
		name            string
		err             error
		wantDisposition autoStopDispatchDisposition
		wantTerminal    bool
		wantRetry       bool
	}{
		{name: "success"},
		{name: "idempotency conflict clears cycle", err: dbstore.ErrIdempotencyConflict, wantDisposition: autoStopDispatchConflict},
		{name: "missing Runtime clears cycle", err: dbstore.ErrReferenceNotOwned, wantDisposition: autoStopDispatchReferenceUnavailable},
		{name: "other permanent failure terminates", err: permanent, wantTerminal: true},
		{name: "Operation conflict retries", err: dbstore.ErrOperationConflict, wantRetry: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			disposition, err := classifyAutoStopDispatchError(test.err)
			if disposition != test.wantDisposition {
				t.Fatalf("disposition = %q, want %q", disposition, test.wantDisposition)
			}
			if restate.IsTerminalError(err) != test.wantTerminal {
				t.Fatalf("terminal error = %t (%v), want %t", restate.IsTerminalError(err), err, test.wantTerminal)
			}
			if test.wantRetry && (!errors.Is(err, test.err) || restate.IsTerminalError(err)) {
				t.Fatalf("retry error = %T %v, want original non-terminal error", err, err)
			}
			if !test.wantRetry && !test.wantTerminal && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
		})
	}
}

type autoStopDispatchTestError struct {
	err       error
	transient bool
}

func (err autoStopDispatchTestError) Error() string   { return err.err.Error() }
func (err autoStopDispatchTestError) Unwrap() error   { return err.err }
func (err autoStopDispatchTestError) Transient() bool { return err.transient }
