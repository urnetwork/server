package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

const proberPreferredGrantResource = "prober_preferred_grants.yml"
const proberPreferredGrantLimit = 16

// This is an explicit, process-lifetime routing hint, never a balance cache.
// It cannot grant credit, change identity, or make an ordinary payer eligible.
type proberPreferredGrantConfig struct {
	NetworkId  server.Id
	BalanceIds []server.Id
}

func loadProberPreferredGrantConfig() (*proberPreferredGrantConfig, error) {
	resource, err := server.Config.SimpleResource(proberPreferredGrantResource)
	if errors.Is(err, server.ErrResourceNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("preferred prober grant configuration unavailable")
	}
	var settings struct {
		Version    int      `yaml:"version"`
		Enabled    bool     `yaml:"enabled"`
		NetworkId  string   `yaml:"network_id"`
		BalanceIds []string `yaml:"balance_ids"`
	}
	if err := resource.UnmarshalYamlE(&settings); err != nil {
		return nil, fmt.Errorf("invalid preferred prober grant configuration")
	}
	if !settings.Enabled {
		return nil, nil
	}
	if settings.Version != 1 || len(settings.BalanceIds) == 0 || proberPreferredGrantLimit < len(settings.BalanceIds) {
		return nil, fmt.Errorf("preferred prober grants require version 1 and 1..%d grants", proberPreferredGrantLimit)
	}
	networkId, err := server.ParseId(settings.NetworkId)
	if err != nil || networkId == (server.Id{}) {
		return nil, fmt.Errorf("invalid preferred prober payer")
	}
	config := &proberPreferredGrantConfig{NetworkId: networkId}
	seen := map[server.Id]bool{}
	for _, raw := range settings.BalanceIds {
		balanceId, err := server.ParseId(raw)
		if err != nil || balanceId == (server.Id{}) || seen[balanceId] {
			return nil, fmt.Errorf("invalid or duplicate preferred prober grant")
		}
		seen[balanceId] = true
		config.BalanceIds = append(config.BalanceIds, balanceId)
	}
	return config, nil
}

var proberPreferredGrants = sync.OnceValue(func() *proberPreferredGrantConfig {
	config, err := loadProberPreferredGrantConfig()
	if err != nil {
		// Deliberately omit configuration values and parser error bodies.
		glog.Errorf("[escrow]preferred prober grants disabled: %s\n", err)
		return nil
	}
	return config
})

var proberPreferredGrantResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_prober_preferred_grant_total",
	Help: "Configured internal-prober positive-byte allocations by preferred grant outcome; never a count of completed probes.",
}, []string{"result"})

func init() {
	prometheus.MustRegister(proberPreferredGrantResults)
}

type escrowTransferBalance struct {
	balanceId        server.Id
	paid             bool
	balanceByteCount ByteCount
	startTime        time.Time
	endTime          time.Time
}

// OFFSET 0 makes the primary-key probe structural. With stale cardinality
// estimates, combining network/time predicates directly with an ANY list can
// choose the expensive all-grants network index instead. The bounded outer
// filters still validate every returned row in this allocation's snapshot.
const proberPreferredGrantSQL = `
	SELECT selected.balance_id, selected.paid, selected.balance_byte_count,
		selected.start_time, selected.end_time
	FROM unnest($1::uuid[]) AS requested(balance_id)
	CROSS JOIN LATERAL (
		SELECT balance_id, network_id, active, start_time, end_time,
			balance_byte_count, start_balance_byte_count, paid, pro,
			net_revenue_nano_cents, subsidy_net_revenue_nano_cents
		FROM transfer_balance
		WHERE balance_id = requested.balance_id
		OFFSET 0
	) AS selected
	WHERE selected.network_id = $2 AND selected.active
		AND selected.start_time <= $3 AND $3 < selected.end_time
		AND NOT selected.paid AND NOT selected.pro
		AND selected.net_revenue_nano_cents = 0
		AND selected.subsidy_net_revenue_nano_cents = 0
		AND selected.start_balance_byte_count >= $4
		AND EXISTS (SELECT 1 FROM prober_identity WHERE singleton AND network_id = $2)
`

// A positive-byte prober request may use one explicitly configured free grant
// ahead of older grants. It must be fully funded on its own; otherwise the
// caller runs the unchanged earliest-expiry allocator from scratch. No partial
// reservation, mirror write, or cached spendability escapes this function.
func preferredProberTransferBalance(
	ctx context.Context, tx server.PgTx,
	payerNetworkId, proberClientId server.Id,
	now time.Time, requestedBytes ByteCount,
) *escrowTransferBalance {
	config := proberPreferredGrants()
	if requestedBytes <= 0 || config == nil || config.NetworkId != payerNetworkId {
		return nil
	}
	outcome := "error"
	defer func() { proberPreferredGrantResults.WithLabelValues(outcome).Inc() }()
	balances := map[server.Id]*escrowTransferBalance{}
	rows, err := tx.Query(ctx, proberPreferredGrantSQL, config.BalanceIds, payerNetworkId, now, ProberTransferBalanceTopUp)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			balance := &escrowTransferBalance{}
			server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount, &balance.startTime, &balance.endTime))
			balances[balance.balanceId] = balance
		}
	})
	if len(balances) == 0 {
		outcome = "fallback_ineligible"
		return nil
	}
	server.Redis(ctx, func(r server.RedisClient) {
		commands := map[server.Id]*redis.StringCmd{}
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for id := range balances {
				commands[id] = pipe.Get(ctx, netEscrowKey(id))
			}
			return nil
		})
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		for id, balance := range balances {
			reserved, err := commands[id].Int64()
			if errors.Is(err, redis.Nil) {
				reserved = 0
			} else {
				server.Raise(err)
			}
			balance.balanceByteCount = max(0, balance.balanceByteCount-max(int64(0), reserved))
		}
	})
	start := int(proberClientId.Hash() % uint64(len(config.BalanceIds)))
	for offset := range config.BalanceIds {
		id := config.BalanceIds[(start+offset)%len(config.BalanceIds)]
		if balance := balances[id]; balance != nil && requestedBytes <= balance.balanceByteCount {
			outcome = "selected"
			return balance
		}
	}
	outcome = "fallback_reserved"
	return nil
}
