package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Acceptance-test balance drain. See model/test_balance_drain_model.go for the
// gate (vault allowlist, or a sign-in email on a tests.yml bypass domain) and
// mechanism. The args deliberately carry no network
// id: the drained network is always the caller's own `session.ByJwt.NetworkId`.

type TestBalanceDrainArgs struct {
	// optional; clamped to [1m, 30m], default 15m
	DurationSeconds int `json:"duration_seconds,omitempty"`
}

type TestBalanceDrainResult struct {
	DrainId                 server.Id       `json:"drain_id"`
	StartTime               time.Time       `json:"start_time"`
	EndTime                 time.Time       `json:"end_time"`
	DrainedBalanceByteCount model.ByteCount `json:"drained_balance_byte_count"`
}

type TestBalanceRestoreArgs struct {
}

type TestBalanceRestoreResult struct {
	RestoredCount int64 `json:"restored_count"`
}

// testBalanceDrainStore is the model surface, injectable for unit tests.
type testBalanceDrainStore interface {
	Drain(ctx context.Context, networkId server.Id, duration time.Duration) (*model.TestBalanceDrain, error)
	Restore(ctx context.Context, networkId server.Id) int64
}

type modelTestBalanceDrainStore struct{}

func (modelTestBalanceDrainStore) Drain(ctx context.Context, networkId server.Id, duration time.Duration) (*model.TestBalanceDrain, error) {
	return model.DrainTestBalance(ctx, networkId, duration)
}

func (modelTestBalanceDrainStore) Restore(ctx context.Context, networkId server.Id) int64 {
	return model.RestoreTestBalance(ctx, networkId)
}

var testBalanceDrainStoreImpl testBalanceDrainStore = modelTestBalanceDrainStore{}

func TestBalanceDrain(args *TestBalanceDrainArgs, clientSession *session.ClientSession) (*TestBalanceDrainResult, error) {
	return testBalanceDrain(testBalanceDrainStoreImpl, model.TestBalanceDrainAllowed, args, clientSession)
}

func TestBalanceRestore(args *TestBalanceRestoreArgs, clientSession *session.ClientSession) (*TestBalanceRestoreResult, error) {
	return testBalanceRestore(testBalanceDrainStoreImpl, clientSession)
}

func testBalanceDrain(
	store testBalanceDrainStore,
	allowed func(context.Context, server.Id) bool,
	args *TestBalanceDrainArgs,
	clientSession *session.ClientSession,
) (*TestBalanceDrainResult, error) {
	if clientSession == nil || clientSession.ByJwt == nil {
		return nil, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
	}
	networkId := clientSession.ByJwt.NetworkId
	// refuse before touching the store; the model checks again
	if !allowed(clientSession.Ctx, networkId) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, model.ErrTestBalanceDrainNotAllowed.Error())
	}
	duration := time.Duration(0)
	if args != nil && 0 < args.DurationSeconds {
		duration = time.Duration(args.DurationSeconds) * time.Second
	}
	drain, err := store.Drain(clientSession.Ctx, networkId, duration)
	if errors.Is(err, model.ErrTestBalanceDrainNotAllowed) {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return &TestBalanceDrainResult{
		DrainId:                 drain.DrainId,
		StartTime:               drain.StartTime,
		EndTime:                 drain.EndTime,
		DrainedBalanceByteCount: drain.DrainedBalanceByteCount,
	}, nil
}

func testBalanceRestore(store testBalanceDrainStore, clientSession *session.ClientSession) (*TestBalanceRestoreResult, error) {
	if clientSession == nil || clientSession.ByJwt == nil {
		return nil, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
	}
	return &TestBalanceRestoreResult{
		RestoredCount: store.Restore(clientSession.Ctx, clientSession.ByJwt.NetworkId),
	}, nil
}
