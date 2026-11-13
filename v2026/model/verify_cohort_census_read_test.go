package model

// Only a missing census table means no durable census can exist; every other
// census read failure fails closed (verifyCohortCensusRequired).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestVerifyCohortCensusTableMissing(t *testing.T) {
	missing := &pgconn.PgError{Code: pgerrcode.UndefinedTable, Message: `relation "verify_cohort_census" does not exist`}
	if !verifyCohortCensusTableMissing(missing) || !verifyCohortCensusTableMissing(fmt.Errorf("census read: %w", missing)) {
		t.Fatal("a missing census table was not recognized")
	}
	for _, err := range []error{
		&pgconn.PgError{Code: pgerrcode.UndefinedColumn},
		&pgconn.PgError{Code: pgerrcode.InsufficientPrivilege},
		&pgconn.PgError{Code: pgerrcode.AdminShutdown},
		errors.New(`relation "verify_cohort_census" does not exist`),
		context.DeadlineExceeded,
	} {
		if verifyCohortCensusTableMissing(err) {
			t.Fatal("a census read failure counted as a missing table", err)
		}
	}
}
