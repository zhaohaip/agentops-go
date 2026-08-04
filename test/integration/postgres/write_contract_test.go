package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/zhaohaip/agentops-go/internal/adapter/postgres/migration"
	postgresruntime "github.com/zhaohaip/agentops-go/internal/adapter/postgres/runtime"
	"github.com/zhaohaip/agentops-go/internal/contracts"
	"github.com/zhaohaip/agentops-go/test/fixtures/writecontract"
	"github.com/zhaohaip/agentops-go/test/integration/postgres/postgrestest"
)

func TestContractsOnlyServiceUsesRealExecutorAndSharedTransaction(t *testing.T) {
	environment := postgrestest.NewRepositoryEnvironment(t, []migration.Migration{{
		Version:    1,
		Name:       "create_write_contract_probe",
		Statements: []string{"CREATE TABLE write_contract_probe (value TEXT NOT NULL)"},
	}})
	first := &postgresContractRepository{}
	second := &postgresContractRepository{}
	service := writecontract.NewService(environment.Runtime.WriteExecutor(), first, second)

	if err := service.StorePair(context.Background(), "first", "second"); err != nil {
		t.Fatalf("StorePair() error = %v", err)
	}
	if first.token == nil || first.token != second.token {
		t.Fatal("PostgreSQL Repositories did not receive the same opaque transaction token")
	}

	var count int
	if err := environment.Runtime.ReadPool().QueryRow(
		context.Background(),
		"SELECT count(*) FROM write_contract_probe",
	).Scan(&count); err != nil {
		t.Fatalf("query committed Repository operations: %v", err)
	}
	if count != 2 {
		t.Fatalf("committed Repository rows = %d, want 2", count)
	}
}

func TestContractsOnlyServiceRepositoryErrorRollsBackAllOperations(t *testing.T) {
	wantErr := errors.New("second repository failed")
	environment := postgrestest.NewRepositoryEnvironment(t, []migration.Migration{{
		Version:    1,
		Name:       "create_write_contract_rollback_probe",
		Statements: []string{"CREATE TABLE write_contract_probe (value TEXT NOT NULL)"},
	}})
	service := writecontract.NewService(
		environment.Runtime.WriteExecutor(),
		&postgresContractRepository{},
		&postgresContractRepository{err: wantErr},
	)

	if err := service.StorePair(context.Background(), "first", "second"); !errors.Is(err, wantErr) {
		t.Fatalf("StorePair() error = %v, want %v", err, wantErr)
	}
	var count int
	if err := environment.Runtime.ReadPool().QueryRow(
		context.Background(),
		"SELECT count(*) FROM write_contract_probe",
	).Scan(&count); err != nil {
		t.Fatalf("query rolled-back Repository operations: %v", err)
	}
	if count != 0 {
		t.Fatalf("rows after Repository error = %d, want 0", count)
	}
}

type postgresContractRepository struct {
	token contracts.RuntimeWriteTx
	err   error
}

func (r *postgresContractRepository) Store(
	ctx context.Context,
	token contracts.RuntimeWriteTx,
	value string,
) error {
	r.token = token
	return postgresruntime.WithPostgreSQLWriteTx(token, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO write_contract_probe (value) VALUES ($1)", value); err != nil {
			return err
		}
		return r.err
	})
}

var _ writecontract.Repository = (*postgresContractRepository)(nil)
