package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The route witness carries no endpoint, credential, account or customer data.
// Root compares its postmaster clock with the independently native Main read.
type contractExpiryRouteWitness struct {
	PostgresReadOnly bool      `json:"postgres_read_only"`
	PostgresPrimary  bool      `json:"postgres_primary"`
	PostgresVersion  int       `json:"postgres_version"`
	PostmasterTime   time.Time `json:"postmaster_time"`
	RedisPing        bool      `json:"redis_ping"`
}

// inspectContractExpiryRoute uses the same selected config/vault and service as
// the repair command. It performs only one read-only PG query and Redis PING.
func inspectContractExpiryRoute(ctx context.Context) (witness contractExpiryRouteWitness, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
		var recovering bool
		var readOnly string
		server.Raise(tx.QueryRow(ctx, `SELECT pg_is_in_recovery(), current_setting('server_version_num')::integer,
			pg_postmaster_start_time(), current_setting('transaction_read_only')`).Scan(
			&recovering, &witness.PostgresVersion, &witness.PostmasterTime, &readOnly))
		witness.PostgresPrimary = !recovering
		witness.PostgresReadOnly = readOnly == "on"
	}, server.TxReadCommitted, pgx.ReadOnly, server.OptNoRetry())
	if !witness.PostgresReadOnly || !witness.PostgresPrimary || witness.PostgresVersion/10000 != 18 {
		return witness, errors.New("expiry_route_refused")
	}
	returnErr = server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		pong, err := client.Ping(ctx).Result()
		witness.RedisPing = err == nil && pong == "PONG"
		if err == nil && !witness.RedisPing {
			return errors.New("expiry_route_refused")
		}
		return err
	})
	return
}

// runContractExpiryRouteCheck keeps route failures finite and catches raw
// configuration/authentication errors before writing its private envelope.
func runContractExpiryRouteCheck(parent context.Context, writer io.Writer,
	inspect func(context.Context) (contractExpiryRouteWitness, error),
) (exitCode int) {
	status := "route_unavailable"
	var witness *contractExpiryRouteWitness
	started := time.Now().UTC()
	defer func() {
		if recover() != nil {
			status = "route_unavailable"
			witness = nil
			exitCode = 1
		}
		err := json.NewEncoder(writer).Encode(struct {
			Schema      int                         `json:"schema"`
			Kind        string                      `json:"kind"`
			StartedAt   time.Time                   `json:"started_at"`
			CompletedAt time.Time                   `json:"completed_at"`
			Status      string                      `json:"status"`
			Witness     *contractExpiryRouteWitness `json:"witness"`
		}{1, "customer-expiry-route-private-v1", started, time.Now().UTC(), status, witness})
		if err != nil {
			exitCode = 1
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return 1
	}
	value, err := inspect(ctx)
	if err != nil || !value.PostgresReadOnly || !value.PostgresPrimary || value.PostgresVersion/10000 != 18 || !value.RedisPing || value.PostmasterTime.IsZero() {
		return 1
	}
	witness = &value
	status = "route_observed"
	return 0
}
