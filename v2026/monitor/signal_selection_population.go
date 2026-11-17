package monitor

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	providerEligibilityReadyKey                  = "client_score_provider_eligibility_v1_ready"
	providerEligibilityReadyValue                = "1"
	selectionPopulationFamilyReadyKey            = "client_score_ip_family_v1_ready"
	selectionPopulationFamilyReadyValue          = "1"
	selectionPopulationRedisQualificationTimeout = 60 * time.Second
)

// SIGNALS.md §2.9 maps to signal_selection_population.go and signal_selection_population_test.go.
func NewSelectionPopulationSignal() Signal {
	return &signalAdapter{number: "2.9", key: "selection-population", name: "Provider-selection population", probe: pgSelectionPopulationProbe{}}
}

// The optional factory is private to deterministic qualification-lifecycle tests.
// Ordinary probes always retain the fixed shared sixty-second child budget.
type pgSelectionPopulationProbe struct {
	redisQualificationContext func(context.Context) (context.Context, context.CancelFunc)
}

func (pgSelectionPopulationProbe) id() string             { return "pg/selection-empty" }
func (pgSelectionPopulationProbe) tier() string           { return tierPage }
func (pgSelectionPopulationProbe) cadence() time.Duration { return 5 * time.Minute }

// Keeps the existing PostgreSQL gates and qualifies only the sampled Redis market.
func (self pgSelectionPopulationProbe) check(ctx context.Context, env *probeEnv) (result []finding, resultErr error) {
	defer finishHandlerLeaseObservation(ctx, &result, &resultErr)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	redisHost := env.cfg.redisEntryHost()
	if redisHost == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	schemaRows, err := env.runner.pg(ctx, `
		SELECT EXISTS (
		 SELECT 1 FROM pg_attribute
		 WHERE attrelid='provider_egress_health'::regclass
		   AND attname='tls_authentication_failure'
		   AND NOT attisdropped
		);
	`)
	if err := handlerLeaseQueryError(ctx, err); err != nil {
		return nil, err
	}
	schemaRow, err := pgAggregateRow(schemaRows, 1)
	if err != nil {
		return nil, err
	}
	tlsIntegrityArmed, err := providerPgBool(schemaRow.str(0))
	if err != nil {
		return nil, fmt.Errorf("invalid response for provider population: invalid TLS-integrity arming state")
	}
	tlsPassingPredicate := ""
	tlsFailureCount := "0"
	if tlsIntegrityArmed {
		// Keep the pre-migration query parseable while allowing PostgreSQL to use
		// the partial boolean index once the append-only column is present. Row
		// conversion through to_jsonb would be schema-compatible but would also
		// serialize every wide health row on every five-minute observation.
		tlsPassingPredicate = "AND NOT peh.tls_authentication_failure"
		tlsFailureCount = "(SELECT count(*) FROM provider_egress_health WHERE tls_authentication_failure)"
	}
	rows, err := env.runner.pg(ctx, fmt.Sprintf(`
		WITH supply AS MATERIALIZED (
		 SELECT nc.active, nc.source_client_id
		 FROM network_client_location_reliability nclr
		 INNER JOIN network_client nc USING (client_id)
		 WHERE nclr.connected AND nclr.valid
		   AND EXISTS (
		    SELECT 1 FROM provide_key pk
		    WHERE pk.client_id=nclr.client_id AND pk.provide_mode IN (1,3)
		   )
		), lease_coverage AS MATERIALIZED (
		 SELECT COUNT(*) AS potential_open_provider_rows,
		  COUNT(*) FILTER (WHERE h.handler_id IS NOT NULL AND h.heartbeat_time < now() - interval '2 minutes') AS stale_open_provider_rows
		 FROM network_client_connection ncc
		 INNER JOIN network_client nc USING (client_id)
		 LEFT JOIN network_client_handler h USING (handler_id)
		 WHERE ncc.connected AND nc.active AND nc.source_client_id IS NULL
		  AND EXISTS (SELECT 1 FROM provide_key pk WHERE pk.client_id=ncc.client_id AND pk.provide_mode IN (1,3))
		)
		SELECT
		 (SELECT count(DISTINCT ncc.client_id) FROM network_client_connection ncc
		  WHERE ncc.connected AND EXISTS (SELECT 1 FROM provide_key pk WHERE pk.client_id=ncc.client_id AND pk.provide_mode=3)),
		 (SELECT count(*) FROM supply),
		 (SELECT count(*) FROM supply WHERE active AND source_client_id IS NULL),
		 (SELECT count(*) FROM supply WHERE source_client_id IS NOT NULL),
		 (SELECT count(*) FROM supply WHERE NOT active AND source_client_id IS NULL),
		 (SELECT count(*) FROM provider_egress_health),
		 (SELECT count(*) FROM provider_egress_health peh
		  WHERE measured_at>=now()-interval '24 hours'
		    AND total_count>0 AND 10*ok_count>=9*total_count
		    %s),
		 (SELECT count(*) FROM provider_egress_location),
		 %s,
		 (SELECT location_id::text FROM location WHERE location_type='country' AND country_code='us' LIMIT 1),
		 (SELECT potential_open_provider_rows FROM lease_coverage),
		 (SELECT stale_open_provider_rows FROM lease_coverage);
	`, tlsPassingPredicate, tlsFailureCount))
	if err := handlerLeaseQueryError(ctx, err); err != nil {
		return nil, err
	}
	row, err := pgAggregateRow(rows, 12)
	if err != nil {
		return nil, err
	}
	var counts [9]int64
	for i := range counts {
		counts[i], err = providerPgInteger(row.str(i), 64, 0)
		if err != nil {
			return nil, err
		}
	}
	// The three supply cohorts partition the same snapshot. Health predicates
	// select subsets; the TLS-excluding passing and failure sets are disjoint.
	if counts[2] > counts[1] || counts[3] > counts[1]-counts[2] ||
		counts[4] != counts[1]-counts[2]-counts[3] ||
		counts[6] > counts[5] || counts[8] > counts[5] ||
		tlsIntegrityArmed && counts[6] > counts[5]-counts[8] ||
		!tlsIntegrityArmed && counts[8] != 0 {
		return nil, fmt.Errorf("invalid response for provider PostgreSQL population relationships")
	}
	potentialOpen, err := providerPgInteger(row.str(10), 64, 0)
	if err != nil {
		return nil, err
	}
	staleOpen, err := providerPgInteger(row.str(11), 64, 0)
	if err != nil {
		return nil, err
	}
	if staleOpen > potentialOpen {
		return nil, fmt.Errorf("invalid response for provider handler lease coverage: inconsistent counts")
	}
	connected, rawSupply, eligible := counts[0], counts[1], counts[2]
	derived, inactive, tlsAuthenticationFailures := counts[3], counts[4], counts[8]
	targetLocation := row.str(9)
	if !providerPgLocationPattern.MatchString(targetLocation) {
		return nil, fmt.Errorf("invalid response for provider PostgreSQL target location")
	}
	caller := "00000000-0000-0000-0000-000000000000"
	normalKey := fmt.Sprintf("{cs_0_q_%s_%s}c_l", caller, targetLocation)
	forcedKey := fmt.Sprintf("{cs_1_q_%s_%s}c_l", caller, targetLocation)
	// The existing rollout markers and count documents share one bounded
	// qualification deadline, not a fresh full timeout for every Redis read.
	newRedisContext := self.redisQualificationContext
	if newRedisContext == nil {
		newRedisContext = func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, selectionPopulationRedisQualificationTimeout)
		}
	}
	redisCtx, cancelRedis := newRedisContext(ctx)
	defer cancelRedis()
	eligibilityMarker, eligibilityErr := env.runner.redis(
		redisCtx, redisHost, redisHost.redisEntryPort,
		"-c", "--raw", "GET", providerEligibilityReadyKey,
	)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	eligibilityKnown := eligibilityErr == nil && redisCtx.Err() == nil
	eligibilityFailureDetails := ""
	if !eligibilityKnown {
		eligibilityFailureDetails = providerPopulationRedisFailureDetails(redisCtx, "eligibility", eligibilityErr)
	}
	eligibilityReady := eligibilityKnown && strings.TrimSpace(eligibilityMarker) == providerEligibilityReadyValue
	eligibilityState := "unavailable"
	if eligibilityKnown {
		eligibilityState = strconv.FormatBool(eligibilityReady)
	}
	// Prefer any present disjoint facet representation; only absent facets
	// and an absent family marker permit the available legacy pair.
	cache := func() providerPopulationCacheObservation {
		unknown := func(phase string, err error) providerPopulationCacheObservation {
			return providerPopulationCacheObservation{
				state:          "source-unobservable",
				failureDetails: providerPopulationRedisFailureDetails(redisCtx, phase, err),
			}
		}
		if redisCtx.Err() != nil {
			return unknown("cache-precheck", nil)
		}
		before, err := env.runner.redis(redisCtx, redisHost, redisHost.redisEntryPort, "-c", "--raw", "GET", selectionPopulationFamilyReadyKey)
		if err != nil || redisCtx.Err() != nil {
			return unknown("family-before", err)
		}
		before = strings.TrimSpace(before)
		if before != "" && before != selectionPopulationFamilyReadyValue {
			return providerPopulationCacheObservation{state: "marker-invalid"}
		}
		observation := providerPopulationCacheObservation{schema: "family-facets"}
		present := 0
		incomplete := false
		for _, forced := range []bool{false, true} {
			key := normalKey
			if forced {
				key = forcedKey
			}
			for _, facet := range []string{"d", "4", "6"} {
				raw, readErr := env.runner.redisRaw(redisCtx, redisHost, redisHost.redisEntryPort, "-c", "--raw", "GET", key+"_"+facet)
				if readErr != nil || redisCtx.Err() != nil {
					return unknown("facet", readErr)
				}
				if providerCountPayloadMissing([]byte(raw)) {
					incomplete = true
					continue
				}
				present++
				count, decodeErr := decodeProviderCount([]byte(raw))
				if decodeErr != nil {
					observation.state = "counts-invalid"
					continue
				}
				if forced {
					if count > int(^uint(0)>>1)-observation.forcedCount {
						observation.state = "counts-invalid"
					} else {
						observation.forcedCount += count
					}
					observation.forcedBytes += len(raw)
				} else {
					if count > int(^uint(0)>>1)-observation.normalCount {
						observation.state = "counts-invalid"
					} else {
						observation.normalCount += count
					}
					observation.normalBytes += len(raw)
				}
			}
		}
		if present == 0 && before == "" {
			observation = providerPopulationCacheObservation{schema: "legacy"}
			for _, forced := range []bool{false, true} {
				key := normalKey
				if forced {
					key = forcedKey
				}
				raw, readErr := env.runner.redisRaw(redisCtx, redisHost, redisHost.redisEntryPort, "-c", "--raw", "GET", key)
				if readErr != nil || redisCtx.Err() != nil {
					return unknown("legacy", readErr)
				}
				if providerCountPayloadMissing([]byte(raw)) {
					observation.state = "cache-missing"
					continue
				}
				count, decodeErr := decodeProviderCount([]byte(raw))
				if decodeErr != nil {
					observation.state = "counts-invalid"
					continue
				}
				if forced {
					observation.forcedCount, observation.forcedBytes = count, len(raw)
				} else {
					observation.normalCount, observation.normalBytes = count, len(raw)
				}
			}
		} else if incomplete && observation.state == "" {
			observation.state = "facets-incomplete"
		}
		after, err := env.runner.redis(redisCtx, redisHost, redisHost.redisEntryPort, "-c", "--raw", "GET", selectionPopulationFamilyReadyKey)
		if err != nil || redisCtx.Err() != nil {
			return unknown("family-after", err)
		}
		after = strings.TrimSpace(after)
		if after != "" && after != selectionPopulationFamilyReadyValue {
			observation.state = "marker-invalid"
		} else if after != before {
			observation.state = "schema-changed"
		}
		return observation
	}()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	normalCount, forcedCount := cache.normalCount, cache.forcedCount
	cacheCounts := "normal_cache_count=unavailable forced_cache_count=unavailable"
	if cache.state == "" {
		cacheCounts = fmt.Sprintf("normal_cache_count=%d forced_cache_count=%d", normalCount, forcedCount)
	}

	findings := []finding{}
	if staleOpen > 0 {
		findings = append(findings, handlerLeaseCoverageFinding(self.id(), pgTarget(env), "potential-provider-connections", staleOpen))
	}
	if eligibilityKnown && !eligibilityReady && derived+inactive > 0 {
		findings = append(findings, finding{
			probeId: "pg/selection-empty", tier: tierPage,
			class: "provider-supply-ineligible", target: pgTarget(env), frame: "legacy-filter", sustain: 1,
			symptom: fmt.Sprintf(
				"provider-score input contains %d derived and %d inactive providing clients without a completed eligibility-filter export",
				derived,
				inactive,
			),
			mechanism: "The legacy score queries trusted connected location rows without joining the durable client lifecycle. Derived window identities and inactive top-level clients could therefore be exported as provider supply; feeding short-lived consumer identities back into destination selection amplifies replacement churn. A completed current writer publishes the durable marker only after filtering both classes from every location and location-group export.",
			baseline:  "Only active top-level clients enter provider-score caches, and client_score_provider_eligibility_v1_ready=1 proves a complete filtered export.",
			observed: fmt.Sprintf(
				"raw_providing_supply=%d eligible_active_top_level=%d derived_providing=%d inactive_top_level_providing=%d eligibility_ready=%t %s tls_integrity_armed=%t tls_authentication_failures=%d",
				rawSupply,
				eligible,
				derived,
				inactive,
				eligibilityReady,
				cacheCounts,
				tlsIntegrityArmed,
				tlsAuthenticationFailures,
			),
			evidence: "PostgreSQL aggregates connected/valid clients holding Network or Public provide keys by active and source-client state. Redis supplies the fixed rollout marker and only qualified aggregate decoded cache counts; unavailable cache counts remain explicitly unavailable. No client identifier leaves either source.",
			context:  "Raw derived or inactive rows may remain for history after the fix, so their existence alone must not keep this alert open once a current, fully converged Taskworker completes the filtered export. The marker does not replace runtime provenance: an old Taskworker can still overwrite caches during a partial rollout.",
			action:   "Deploy every Taskworker from a Server descendant of b7599962 that contains the provider-eligibility ready marker. Let the existing serialized UpdateClientScores task complete and publish the marker; do not delete client, location, score-cache, or provide-key state manually.",
			verify:   "Every active Taskworker has the required ancestry, one post-convergence UpdateClientScores run completes, the marker equals 1, bounded cache samples contain only active top-level clients, and §2.7 child churn plus destination diversity recover for two mature cohorts.",
			playbook: "SIGNALS.md §2.9 and §2.16",
		})
	} else if eligibilityKnown {
		findings = append(findings, healthyFinding(
			"pg/selection-empty", tierPage, "provider-supply-ineligible", pgTarget(env),
		))
	} else {
		visibility := providerPopulationVisibilityFinding(pgTarget(env), "eligibility-marker-unobservable")
		visibility.frame = "eligibility-observation"
		visibility.symptom = "The monitor could not observe the provider-eligibility export marker"
		visibility.observed = fmt.Sprintf("eligibility_marker_known=false cache_population_known=%t raw_payload_rendered=false %s", cache.state == "", eligibilityFailureDetails)
		findings = append(findings, visibility)
	}
	if cache.state != "" {
		visibility := providerPopulationVisibilityFinding(pgTarget(env), cache.state)
		if cache.failureDetails != "" {
			visibility.observed += " " + cache.failureDetails
		}
		findings = append(findings, visibility)
		return findings, nil
	}
	if eligible <= 1000 || normalCount > 0 {
		if staleOpen == 0 {
			findings = append(findings, healthyFinding("pg/selection-empty", tierPage, "selection-empty", pgTarget(env)))
		}
		return findings, nil
	}
	mode := "upstream-empty"
	mechanism := "Both complete schema-qualified normal and ForceMinimum count documents encode an aggregate count of zero. This observes an empty export at the baseline target; it does not independently establish why supply, location mapping or the writer produced it."
	if forcedCount > 0 {
		mode = "gate-wipe"
		mechanism = "The complete schema-qualified ForceMinimum export is nonzero while the normal export is empty. This isolates the normal-versus-forced output boundary; discriminate the reliability, score and egress predicates before attributing the cause."
	}
	findings = append(findings, finding{
		probeId: "pg/selection-empty", tier: tierPage,
		class: "selection-empty", target: pgTarget(env), frame: mode, sustain: 2,
		symptom:   fmt.Sprintf("eligible score candidates=%d but normal score-cache export=%d (ForceMinimum=%d)", eligible, normalCount, forcedCount),
		mechanism: mechanism,
		baseline:  "The sampled zero-caller baseline market has a nonzero normal provider count when the unchanged eligible-population gate applies. These schema-qualified aggregate exports need not equal global eligible supply and do not prove every caller, location or IP family healthy.",
		observed: fmt.Sprintf("connected=%d raw_supply=%d eligible=%d derived=%d inactive=%d eligibility_ready=%s normal=%d forced=%d egress_health=%s fresh_passing_excluding_tls=%s egress_locations=%s tls_integrity_armed=%t tls_authentication_failures=%d target=%s",
			connected, rawSupply, eligible, derived, inactive, eligibilityState, normalCount, forcedCount, rows[0].str(5), rows[0].str(6), rows[0].str(7), tlsIntegrityArmed, tlsAuthenticationFailures, targetLocation),
		evidence: fmt.Sprintf("cache_schema=%s normal_document_bytes=%d forced_document_bytes=%d; stable family markers qualify complete aggregate counts; TLS-integrity evidence is aggregate-only and the compatibility query does not require the pending column to exist", cache.schema, cache.normalBytes, cache.forcedBytes),
		action:   "Split the score predicates and inspect the deployed provider.yml enable_egress_test value before changing provider connectivity or cache TTLs.",
		verify:   "A fresh UpdateClientScores run produces complete stable-schema documents with a nonzero normal count for the same baseline market. Independently verify eligibility and the owning predicate if a valid zero persists.",
		playbook: "SIGNALS.md §2.9 and §5.9",
	})
	return findings, nil
}

// Retains only qualified aggregate counts and fixed observation states.
type providerPopulationCacheObservation struct {
	failureDetails string
	normalCount    int
	forcedCount    int
	normalBytes    int
	forcedBytes    int
	schema         string
	state          string
}

// Fixed projection distinguishes owner expiry from a source-returned context
// error. Error classes are bounded hints, never native phase or service proof.
func providerPopulationRedisFailureDetails(ctx context.Context, phase string, err error) string {
	childState := "live"
	childErr := ctx.Err()
	if childErr == context.DeadlineExceeded {
		childState = "deadline"
	} else if childErr == context.Canceled {
		childState = "canceled"
	}
	returnedContext := "none"
	if errors.Is(err, context.DeadlineExceeded) {
		returnedContext = "deadline"
	} else if errors.Is(err, context.Canceled) {
		returnedContext = "canceled"
	}
	details := fmt.Sprintf("redis_read_phase=%s redis_child_state=%s redis_returned_context=%s redis_error_class=%s", phase, childState, returnedContext, classifyObservationError(err))
	if phases := sshAdmissionFailurePhases(err); phases != "" {
		details += " redis_ssh_admission_failures=" + phases
	}
	return details
}

// Missing raw GET replies are not complete zero-count documents.
func providerCountPayloadMissing(raw []byte) bool {
	value := strings.TrimSpace(string(raw))
	return value == "" || value == "(nil)"
}

// One complete Gob count array is required, with only the raw CLI newline
// optionally remaining. A negative entry or sum overflow cannot become zero.
func decodeProviderCount(raw []byte) (int, error) {
	if providerCountPayloadMissing(raw) {
		return 0, errors.New("provider count document is unavailable")
	}
	reader := bytes.NewReader(raw)
	var counts []int
	if err := gob.NewDecoder(reader).Decode(&counts); err != nil {
		return 0, errors.New("provider count document is incompatible")
	}
	if remaining := reader.Len(); remaining != 0 && (remaining != 1 || raw[len(raw)-1] != '\n') {
		return 0, errors.New("provider count document has additional data")
	}
	total := 0
	for _, count := range counts {
		if count < 0 || count > int(^uint(0)>>1)-total {
			return 0, errors.New("provider count document has invalid counts")
		}
		total += count
	}
	return total, nil
}

// Unqualified evidence cannot resolve either an empty-market or eligibility incident.
func providerPopulationVisibilityFinding(target, state string) finding {
	f := cannotObserveFinding(target, errors.New("provider population cache qualification unavailable"))
	f.frame = "cache-observation"
	f.symptom = "The monitor could not qualify the provider-selection cache observation"
	f.mechanism = "A missing, partial or incompatible count document, an unobserved marker, or a schema change is source visibility loss; none proves an empty provider market or healthy recovery."
	f.baseline = "A stable observed family schema selects complete valid normal and ForceMinimum documents: all disjoint d/4/6 facets, or an available legacy pair only before the family marker when no facets are present."
	f.observed = fmt.Sprintf("cache_state=%s cache_population_known=false raw_payload_rendered=false", state)
	f.evidence = "The same zero-caller baseline target is read under one 60-second Redis child deadline. Exact schema markers bracket count observations. Raw count documents, keys, marker values and private transport/decoder details are not rendered; absent GET is not an encoded empty array."
	f.context = "The canary samples one baseline target and rank; overall counts do not certify each IP family, caller, location, sampled provider or API release. Independently established eligibility evidence is retained."
	f.action = "Restore source visibility or let the existing complete writer export settle, then repeat the same bounded schema-qualified read. Do not delete caches, change TTLs or mutate provider connectivity on missing data."
	f.verify = "The before/after family schema is stable, the required complete document set is valid, and only then apply the unchanged normal-empty and ForceMinimum discriminator."
	f.playbook = "SIGNALS.md §2.9"
	return f
}
