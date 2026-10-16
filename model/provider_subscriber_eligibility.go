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
	Help: "Subscriber eligibility cache candidate events and SQL batch attempts, and candidates a subscriber read found without a live connection (offline); positive eligibility and successful requests are not cached or counted here.",
}, []string{"event"})

var subscriberEligibilityEventCounters = map[string]prometheus.Counter{}

func init() {
	for _, event := range []string{"negative_hit", "negative_miss", "coalesced_wait", "capacity_bypass", "fresh_reread", "sql_batch", "offline"} {
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

// The exclusions of a request that names its providers by client id and has no
// discovery spec. Named providers keep the common exclusions in every mode. A
// named Quality provider also needs a live connection with no live ARIN risk,
// but not ARIN quality verification (getProviderNamedSubscriberExclusions). A
// named provider is never read for a liveness lease: a redial that names its
// exit gets one try, and a wrong offline verdict there changes the user's IP.
// Discovery, force_minimum included, evaluates the full subscriber evidence
// for native Quality membership; Speed and Online borrowing use the
// independent common exclusions, and leave out a candidate the request's own
// read found without a live connection.
func getProviderRequestExclusions(ctx context.Context, clientIds []server.Id, mode RankMode) (map[server.Id]bool, error) {
	excluded, err := getProviderHardExclusions(ctx, clientIds)
	if err != nil || mode != RankModeQuality || len(clientIds) == 0 {
		return excluded, err
	}
	pending := make([]server.Id, 0, len(clientIds))
	for _, id := range clientIds {
		if !excluded[id] {
			pending = append(pending, id)
		}
	}
	offline, risky, err := getProviderNamedSubscriberExclusions(ctx, pending)
	if err != nil {
		return nil, err
	}
	for id := range offline {
		excluded[id] = true
	}
	for id := range risky {
		excluded[id] = true
	}
	return excluded, nil
}

// Separate Quality membership from explicit live risk. A risk observed by this
// guard remains a common refusal even while the published snapshot is older.
// Quality reads each live row's ARIN slot whose epoch is the published serving
// epoch and refuses a row with neither (arindb_canary_reader.go); risk reads
// the latest slot. A candidate the read finds without any live connection is
// the third result, offline: a Quality refusal, which discovery also keeps out
// of every other draw of the request that read it.
func getProviderSubscriberExclusions(ctx context.Context, clientIds []server.Id) (negative, risky, offline map[server.Id]bool, returnErr error) {
	enabled, err := subscriberQualityPolicyEnabled()
	if err != nil || !enabled {
		providerSubscriberNegativeCache.reset()
		return nil, nil, nil, err
	}
	// read before any database connection is taken; 0 is the latest slot
	servingEpoch := readArindbServingEpoch(ctx)
	// Positive decisions always read current connection facts on the primary.
	// A one-second negative-only cache coalesces repeated rejections; its age
	// starts before the first fact query. It cannot admit a new unknown or risky
	// connection.
	// Candidate chunks bound both the query and result size.
	const chunkSize = providerQualityValidationBatchSize
	pending := make([]server.Id, 0, len(clientIds))
	seen := make(map[server.Id]bool, len(clientIds))
	for _, id := range clientIds {
		if !seen[id] {
			seen[id] = true
			pending = append(pending, id)
		}
	}
	if len(pending) == 0 {
		return nil, nil, nil, nil
	}
	risky = make(map[server.Id]bool)
	offline = make(map[server.Id]bool)
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
				rows, err := conn.Query(ctx, providerSubscriberExclusionsSql, chunk, server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout), servingEpoch)
				if err != nil {
					readErr = err
					return
				}
				for rows.Next() {
					var id server.Id
					var risk, noLiveConnection bool
					if err := rows.Scan(&id, &risk, &noLiveConnection); err != nil {
						rows.Close()
						readErr = err
						return
					}
					if risk {
						risky[id] = true
						// The cache stores Quality-only refusals, not common risk.
						// Omitting this id makes concurrent followers read fresh;
						// it cannot lose the reason on a later cached downgrade.
					} else if noLiveConnection {
						// Nor the absence of a live connection, for the same
						// reason: each request that reads it reads it fresh.
						offline[id] = true
					} else {
						negative[id] = true
					}
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
		return nil, nil, nil, readErr
	}
	observeSubscriberEligibilityEvent("offline", len(offline))
	return negative, risky, offline, nil
}

// Aggregate the candidate batch together so the live-handler relation is joined
// once per batch rather than rescanned by a correlated aggregate per candidate.
// $3 is the serving epoch: 0 reads each row's latest slot as before the canary;
// otherwise the slot whose epoch it is, and a row with neither is not Quality.
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
	SELECT candidates.client_id, bool_or(COALESCE(location.arin_risk, false)) AS arin_risk,
		count(live.connection_id) = 0 AS no_live_connection
	FROM candidates
	LEFT JOIN live ON live.client_id = candidates.client_id
	LEFT JOIN network_client_location AS location ON location.connection_id = live.connection_id
	GROUP BY candidates.client_id
	HAVING count(live.connection_id) = 0 OR bool_and(
		CASE
			WHEN $3::bigint = 0 OR location.arin_database_build_epoch = $3::bigint THEN
				COALESCE(location.arin_quality_verified, false)
				AND NOT COALESCE(location.arin_non_quality, true)
			WHEN location.arin_old_database_build_epoch = $3::bigint THEN
				COALESCE(location.arin_old_quality_verified, false)
				AND NOT COALESCE(location.arin_old_non_quality, true)
			ELSE false
		END
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
