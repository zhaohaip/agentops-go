package migration

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestNewRunnerRejectsMissingConnection(t *testing.T) {
	t.Parallel()

	if _, err := NewRunner(nil, nil); err == nil {
		t.Fatal("NewRunner() error = nil, want connection error")
	}
}

func TestRunnerReportsDatabaseBeginFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("database unavailable")
	runner, err := NewRunner(failingBeginner{err: wantErr}, nil)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	err = runner.Migrate(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Migrate() error = %v, want database error", err)
	}
}

func TestRunnerRejectsNilContext(t *testing.T) {
	t.Parallel()

	runner, err := NewRunner(failingBeginner{err: errors.New("must not be called")}, nil)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	if err := runner.Migrate(nil); err == nil {
		t.Fatal("Migrate() error = nil, want context error")
	}
}

type failingBeginner struct {
	err error
}

func (b failingBeginner) Begin(context.Context) (pgx.Tx, error) {
	return nil, b.err
}
