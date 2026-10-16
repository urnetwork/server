package controller

// Database-backed checks that the refresh grant (AddRefreshTransferBalanceInTx) reads
// the supporter renewal in the caller's transaction. It used to read through a
// separate pooled connection while that transaction was open: the read missed a
// renewal the transaction itself had written, and it held a second connection while
// the transaction held one. Needs the test database and redis (server.DefaultTestEnv).

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A supporter renewal written earlier in the same tx, not yet committed, makes the
// refresh grant the Pro grant.
func TestAddRefreshTransferBalanceInTxSeesItsOwnRenewal(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "refreshrenewal", server.NewId())

		now := server.NowUtc()
		var renewalErr error
		var grantErr error
		proGranted := false
		server.Tx(ctx, func(tx server.PgTx) {
			renewalErr = model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   model.SubscriptionTypeSupporter,
				StartTime:          now.Add(-24 * time.Hour),
				EndTime:            now.Add(29 * 24 * time.Hour),
				NetRevenue:         model.UsdToNanoCents(5),
				SubscriptionMarket: model.SubscriptionMarketManual,
			})
			if renewalErr != nil {
				return
			}
			proGranted, grantErr = AddRefreshTransferBalanceInTx(tx, ctx, networkId)
		})
		connect.AssertEqual(t, renewalErr, nil)
		connect.AssertEqual(t, grantErr, nil)
		connect.AssertEqual(t, proGranted, true)

		transferBalances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(transferBalances), 1)
		connect.AssertEqual(t, transferBalances[0].Pro, true)
		connect.AssertEqual(t, transferBalances[0].GrantKind, model.GrantKindPro)
	})
}

// With the pool cut to one connection, the tx holds that connection for the whole
// grant. A read on any other pooled connection would wait for it until the deadline
// and fail the grant.
func TestAddRefreshTransferBalanceInTxHoldsOneConnection(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := testingCreateSupporterNetwork(t, ctx)

		pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() {
			pop()
			server.PgReset()
		}()

		txCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var grantErr error
		var failure any
		proGranted := false
		func() {
			defer func() {
				failure = recover()
			}()
			server.Tx(txCtx, func(tx server.PgTx) {
				proGranted, grantErr = AddRefreshTransferBalanceInTx(tx, txCtx, networkId)
			})
		}()
		connect.AssertEqual(t, failure, nil)
		connect.AssertEqual(t, grantErr, nil)
		connect.AssertEqual(t, proGranted, true)

		// the size of the default pool as it was opened
		defaultPgPoolMaximum := func() float64 {
			t.Helper()
			families, err := prometheus.DefaultGatherer.Gather()
			if err != nil {
				t.Fatal(err)
			}
			for _, family := range families {
				if family.GetName() != "urnetwork_pg_pool_connections" {
					continue
				}
				for _, metric := range family.Metric {
					labels := map[string]string{}
					for _, label := range metric.Label {
						labels[label.GetName()] = label.GetValue()
					}
					if labels["pool"] == "default" && labels["state"] == "maximum" {
						return metric.GetGauge().GetValue()
					}
				}
			}
			t.Fatal("the default pool size is unavailable")
			return 0
		}
		connect.AssertEqual(t, defaultPgPoolMaximum(), float64(1))

		transferBalances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(transferBalances), 1)
		connect.AssertEqual(t, transferBalances[0].Pro, true)
	})
}
