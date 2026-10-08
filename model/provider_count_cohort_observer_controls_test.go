// Completion controls pin cohort observation independently of a database service.
package model

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Nested unrelated work must not inherit the outer query's observation token.
func TestProviderCountCohortObserverShadowsUnmatchedContext(t *testing.T) {
	observer := &cohortQueryObserver{}
	parent := observer.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: "WITH failed_reliability AS (SELECT 1) SELECT 1 WHERE 1=ANY($1)", Args: []any{"synthetic-private-argument"}})
	child := observer.TraceQueryStart(parent, nil, pgx.TraceQueryStartData{SQL: "SELECT 99"})
	observer.TraceQueryEnd(child, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 99")})
	observer.TraceQueryEnd(parent, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
	calls, rows, unscoped, err := observer.snapshot()
	if calls != 1 || rows != 1 || unscoped != 0 || err != nil {
		t.Fatalf("unrelated nested query inherited cohort ownership: calls=%d rows=%d unscoped=%d err=%v", calls, rows, unscoped, err)
	}
}

// Separate instances cannot merge measurements even with nested traced contexts.
func TestProviderCountCohortObserverInstancesAreIndependent(t *testing.T) {
	first, second := &cohortQueryObserver{}, &cohortQueryObserver{}
	ctx := first.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "WITH failed_reliability AS (SELECT 1) SELECT 1"})
	ctx = second.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "WITH failed_reliability AS (SELECT 1) SELECT 1 WHERE 1=ANY($1)"})
	second.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 2")})
	first.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 3")})
	calls, rows, unscoped, err := first.snapshot()
	if calls != 1 || rows != 3 || unscoped != 1 || err != nil {
		t.Fatal("first observer lost its actual unscoped completion")
	}
	calls, rows, unscoped, err = second.snapshot()
	if calls != 1 || rows != 2 || unscoped != 0 || err != nil {
		t.Fatal("second observer mixed another instance's counters")
	}
}

// Failed, incomplete, and nonselect outcomes never become successful zero rows.
func TestProviderCountCohortObserverRetainsIncompleteAndFailure(t *testing.T) {
	observer := &cohortQueryObserver{}
	ctx := observer.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: "WITH failed_reliability AS (SELECT 1) SELECT 1 WHERE 1=ANY($1)"})
	if _, _, _, err := observer.snapshot(); err == nil {
		t.Fatal("unfinished query was accepted as complete")
	}
	failure := errors.New("synthetic query failure")
	observer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: failure})
	if _, _, _, err := observer.snapshot(); !errors.Is(err, failure) {
		t.Fatal("query failure was erased")
	}
	other := &cohortQueryObserver{}
	ctx = other.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: "WITH failed_reliability AS (SELECT 1) SELECT 1"})
	other.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("UPDATE 1")})
	if _, _, _, err := other.snapshot(); err == nil {
		t.Fatal("nonselect completion passed row observation")
	}
}
