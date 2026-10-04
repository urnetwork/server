package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

var subscriberEligibilityEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_subscriber_eligibility_events_total",
	Help: "Subscriber eligibility cache candidate events and SQL batch attempts; positive eligibility and successful requests are not cached or counted here.",
}, []string{"event"})

var subscriberEligibilityEventCounters = map[string]prometheus.Counter{}

func init() {
	for _, event := range []string{"negative_hit", "negative_miss", "coalesced_wait", "capacity_bypass", "fresh_reread", "sql_batch"} {
		subscriberEligibilityEventCounters[event] = subscriberEligibilityEvents.WithLabelValues(event)
	}
	prometheus.MustRegister(subscriberEligibilityEvents)
	providerSubscriberNegativeCache.observe = observeSubscriberEligibilityEvent
}

func observeSubscriberEligibilityEvent(event string, count int) {
	if counter := subscriberEligibilityEventCounters[event]; counter != nil && count > 0 {
		counter.Add(float64(count))
	}
}

// Subscriber eligibility applies to the original Quality request, including
// named providers, force_minimum, Speed borrowing and Online fallback. The
// common cached hard-exclusion set deliberately has no Quality-only members.
func getProviderRequestExclusions(ctx context.Context, clientIds []server.Id, mode RankMode) (map[server.Id]bool, error) {
	excluded, err := getProviderHardExclusions(ctx, clientIds)
	if err != nil || mode != RankModeQuality || len(clientIds) == 0 {
		return excluded, err
	}
	enabled, err := subscriberQualityPolicyEnabled()
	if err != nil || !enabled {
		providerSubscriberNegativeCache.reset()
		return excluded, err
	}
	// Positive decisions always read current connection facts on the primary.
	// A one-second negative-only cache coalesces repeated rejections; its age
	// starts before the first fact query. It cannot admit a new unknown or risky
	// connection.
	// Candidate chunks bound both the query and result size.
	const chunkSize = 256
	pending := make([]server.Id, 0, len(clientIds))
	seen := make(map[server.Id]bool, len(clientIds))
	for _, id := range clientIds {
		if !excluded[id] && !seen[id] {
			seen[id] = true
			pending = append(pending, id)
		}
	}
	if len(pending) == 0 {
		return excluded, nil
	}
	negative, readErr := providerSubscriberNegativeCache.lookup(ctx, pending, func(ctx context.Context, pending []server.Id, markObserved func()) (map[server.Id]bool, error) {
		negative := make(map[server.Id]bool)
		var readErr error
		server.Db(ctx, func(conn server.PgConn) {
			for start := 0; start < len(pending); start += chunkSize {
				chunk := pending[start:min(start+chunkSize, len(pending))]
				observeSubscriberEligibilityEvent("sql_batch", 1)
				// Connection queueing has read no facts. Start the bounded age
				// immediately before the first query; later chunks cannot renew it.
				markObserved()
				rows, err := conn.Query(ctx, providerSubscriberExclusionsSql, chunk, server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout))
				if err != nil {
					readErr = err
					return
				}
				for rows.Next() {
					var id server.Id
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						readErr = err
						return
					}
					negative[id] = true
				}
				readErr = rows.Err()
				rows.Close()
				if readErr != nil {
					return
				}
			}
		}, server.OptReadOnly(), server.OptNoRetry())
		return negative, readErr
	})
	if readErr != nil {
		return nil, readErr
	}
	for id := range negative {
		excluded[id] = true
	}
	return excluded, nil
}

// Aggregate the candidate batch together so the live-handler relation is joined
// once per batch rather than rescanned by a correlated aggregate per candidate.
const providerSubscriberExclusionsSql = `
	WITH candidates AS MATERIALIZED (
		SELECT DISTINCT unnest($1::uuid[]) AS client_id
	), connections AS MATERIALIZED (
		SELECT connection.client_id, connection.connection_id, connection.handler_id
		FROM candidates
		JOIN network_client_connection AS connection ON connection.client_id = candidates.client_id AND connection.connected
	), live AS MATERIALIZED (
		SELECT connections.client_id, connections.connection_id
		FROM connections
		JOIN network_client_handler AS handler ON handler.handler_id = connections.handler_id
			AND handler.heartbeat_time >= $2
	)
	SELECT candidates.client_id
	FROM candidates
	LEFT JOIN live ON live.client_id = candidates.client_id
	LEFT JOIN network_client_location AS location ON location.connection_id = live.connection_id
	GROUP BY candidates.client_id
	HAVING count(live.connection_id) = 0 OR bool_and(
		COALESCE(location.arin_quality_verified, false)
		AND NOT COALESCE(location.arin_non_quality, true)
		AND NOT COALESCE(location.arin_risk, true)
	) IS DISTINCT FROM true
`

// Activation is separate from merely deploying compatible binaries or acquiring
// new connection evidence. Absence preserves the published legacy policy.
func subscriberQualityPolicyEnabled() (bool, error) {
	return subscriberQualityPolicyEnabledFromResource(server.Config.SimpleResource(providerConfigResourceName))
}

func subscriberQualityPolicyEnabledFromResource(resource *server.SimpleResource, lookupErr error) (bool, error) {
	if errors.Is(lookupErr, server.ErrResourceNotFound) {
		return false, nil
	}
	if lookupErr != nil {
		return false, fmt.Errorf("subscriber quality policy is unavailable: %w", lookupErr)
	}
	if resource == nil {
		return false, errors.New("subscriber quality policy resource is unavailable")
	}
	var document struct {
		Version uint32 `yaml:"subscriber_quality_policy_version"`
	}
	if err := resource.UnmarshalYamlE(&document); err != nil {
		return false, fmt.Errorf("invalid subscriber quality policy configuration: %w", err)
	}
	switch document.Version {
	case 0:
		return false, nil
	case 2:
		return true, nil
	default:
		return false, fmt.Errorf("unsupported subscriber quality policy version: %d", document.Version)
	}
}
