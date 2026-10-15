package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPrivateContractExpiryRepairRouteWitness(t *testing.T) {
	var output bytes.Buffer
	code := runContractExpiryRouteCheck(context.Background(), &output, func(ctx context.Context) (contractExpiryRouteWitness, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 8*time.Second {
			t.Fatal("route context is unbounded")
		}
		return contractExpiryRouteWitness{true, true, 180000, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), true}, nil
	})
	if code != 0 || !strings.Contains(output.String(), `"status":"route_observed"`) {
		t.Fatal("qualified finite route witness was refused")
	}
}

func TestPrivateContractExpiryRepairRouteFailurePrivacy(t *testing.T) {
	for _, panicFailure := range []bool{false, true} {
		var output bytes.Buffer
		code := runContractExpiryRouteCheck(context.Background(), &output, func(context.Context) (contractExpiryRouteWitness, error) {
			if panicFailure {
				panic("synthetic-private-route-error")
			}
			return contractExpiryRouteWitness{}, errors.New("synthetic-private-route-error")
		})
		if code != 1 || strings.Contains(output.String(), "synthetic-private-route-error") || !strings.Contains(output.String(), `"witness":null`) {
			t.Fatal("unqualified route or raw error escaped")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	var output bytes.Buffer
	if runContractExpiryRouteCheck(ctx, &output, func(context.Context) (contractExpiryRouteWitness, error) {
		called = true
		return contractExpiryRouteWitness{}, nil
	}) != 1 || called {
		t.Fatal("canceled route check reached database callback")
	}
}
