// The referral terms past the bonus amounts (owner 2026-11-24): a referral
// pays its two sides only once the referred network uses the app, and only
// for a limited period from its creation.
//
// App use. A link made after the terms began (network_referral.existing_link
// false, the column's default since migration 1010) pays neither side until
// its referred network qualifies: app use of at least AppUseDailyByteCount on
// at least AppUseDayCount distinct UTC days. App use is the data the
// network's clients pay for, through any client but a hosted proxy (a client
// with a proxy_device_config), the windows it mints, and anything else
// descended from one. The usage rollup adds each drained block's app use to
// network_referral_app_day, by the UTC day the block started, for the
// networks that still have to qualify (recordReferralAppUse). Connection
// history is kept for about a day, so the record starts with the terms and
// nothing before them counts. Each grant run first marks the networks whose
// days qualify them (network_referral_referee.qualified_time), so both sides
// earn from the first run after the second qualifying day, and then drops the
// days nothing can count any more. A qualified network stays qualified. A
// referrer counts only the links that pay, still capped at max_referrals.
//
// A link that existed when the terms began (existing_link true) pays without
// app use. A re-point to another referrer makes the link new, so it needs app
// use like any new link.
//
// Period. Every link pays for LinkDuration from its create_time. A re-point
// keeps the link's create_time, and a link made again after an unlink takes
// the time of the network's first link (network_referral_referee
// first_link_time), so neither restarts the period. The links that existed
// when the terms began are held to the period only from
// ExistingLinkLimitFrom, the date the notice of the change sets (terms §11:
// a material change takes effect seven days after its notice).
//
// Between the migration and the release that reads it, the running release
// still writes network_referral itself: its new links take the default and
// are new links, but a re-point it makes keeps existing_link and restarts
// create_time, and an unlink it makes records no first link. That window is
// the deploy's.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// pro.yml referral: an absent key keeps its default. The absent pro.yml
// (ProConfig{}) holds the zero value, which limits nothing, but no referral
// grant runs then.
type ReferralTermsSettings struct {
	// How long a referral pays from its creation (pro.yml
	// referral.link_duration). 90 days (owner 2026-11-24). Non-positive: for
	// life.
	LinkDuration time.Duration
	// From when LinkDuration also limits the links that existed when the
	// terms began (pro.yml referral.existing_link_limit_from, RFC 3339 or a
	// date at 00:00 UTC). Set when the notice of the change goes out, to the
	// end of its notice period. Zero: never, so those links pay for life.
	ExistingLinkLimitFrom time.Time
	// A qualifying day's app use (pro.yml referral.app_use_daily_bytes).
	// 10 MiB (owner 2026-11-24). Non-positive: any recorded app use.
	AppUseDailyByteCount ByteCount
	// How many distinct qualifying UTC days a referred network needs (pro.yml
	// referral.app_use_days). 2 (owner 2026-11-24). Non-positive: none, so
	// every link pays without app use.
	AppUseDayCount int
	// How long a day of app use is kept for a network that has not
	// qualified. A link past LinkDuration pays nothing, so its days count for
	// nothing after that. Non-positive: a day is kept until its network
	// qualifies or the day falls short.
	AppUseRetention time.Duration
	// At most this many days of app use are dropped in one grant run; the
	// rest go in the next. Non-positive: none are dropped.
	AppUseReapLimit int
}

func DefaultReferralTermsSettings() ReferralTermsSettings {
	return ReferralTermsSettings{
		LinkDuration:         90 * 24 * time.Hour,
		AppUseDailyByteCount: 10 * 1024 * 1024,
		AppUseDayCount:       2,
		AppUseRetention:      90 * 24 * time.Hour,
		AppUseReapLimit:      100000,
	}
}

// The defaults with pro.yml's keys applied. A malformed value panics, as
// every other pro.yml value does, rather than silently lift a limit.
func parseReferralTerms(y proReferralYaml) ReferralTermsSettings {
	terms := DefaultReferralTermsSettings()
	if y.LinkDuration != "" {
		terms.LinkDuration = mustParseDuration(y.LinkDuration)
	}
	if y.ExistingLinkLimitFrom != "" {
		limitFrom, err := time.Parse(time.RFC3339, y.ExistingLinkLimitFrom)
		if err != nil {
			limitFrom, err = time.Parse(time.DateOnly, y.ExistingLinkLimitFrom)
		}
		if err != nil {
			panic(fmt.Errorf("pro.yml: invalid referral.existing_link_limit_from %q: %w", y.ExistingLinkLimitFrom, err))
		}
		terms.ExistingLinkLimitFrom = limitFrom.UTC()
	}
	if y.AppUseDailyBytes != "" {
		terms.AppUseDailyByteCount = mustParseByteCount(y.AppUseDailyBytes)
	}
	if y.AppUseDays != nil {
		terms.AppUseDayCount = *y.AppUseDays
	}
	return terms
}

// One referral link, as the grant reads it.
type referralLink struct {
	networkId         server.Id
	referralNetworkId server.Id
	createTime        time.Time
	// the link existed when the terms began and was not re-pointed since
	existingLink bool
	// the referred network's app use qualified it
	qualified bool
}

// Whether the link pays both its sides for the grant window that starts at
// startTime.
func referralLinkPays(link referralLink, startTime time.Time, terms *ReferralTermsSettings) bool {
	if !link.existingLink && 0 < terms.AppUseDayCount && !link.qualified {
		return false
	}
	if terms.LinkDuration <= 0 {
		return true
	}
	// an existing link keeps its terms until the end of the notice period
	if link.existingLink && (terms.ExistingLinkLimitFrom.IsZero() || startTime.Before(terms.ExistingLinkLimitFrom)) {
		return true
	}
	return startTime.Before(link.createTime.Add(terms.LinkDuration))
}

// referralNetworkId -> the networks it referred, by the links that pay for the
// window that starts at startTime.
func payingReferrals(links []referralLink, startTime time.Time, terms *ReferralTermsSettings) map[server.Id][]server.Id {
	referrals := map[server.Id][]server.Id{}
	for _, link := range links {
		if referralLinkPays(link, startTime, terms) {
			referrals[link.referralNetworkId] = append(referrals[link.referralNetworkId], link.networkId)
		}
	}
	return referrals
}

// Every referral link, about one row per referred network. A query failure
// raises, so the grant task retries rather than commit a run that paid no one.
func getReferralLinks(ctx context.Context) []referralLink {
	links := []referralLink{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_referral.network_id,
					network_referral.referral_network_id,
					network_referral.create_time,
					network_referral.existing_link,
					network_referral_referee.qualified_time IS NOT NULL
				FROM network_referral
				LEFT JOIN network_referral_referee ON
					network_referral_referee.network_id = network_referral.network_id
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var link referralLink
				server.Raise(result.Scan(
					&link.networkId,
					&link.referralNetworkId,
					&link.createTime,
					&link.existingLink,
					&link.qualified,
				))
				links = append(links, link)
			}
		})
	})
	return links
}

// Marks the referred networks whose days of app use meet the terms as
// qualified, then drops the days nothing can count any more: those of a
// qualified network, the past days short of a qualifying day, and the days
// past the retention. Days are UTC days, and now's day is still open. Every
// network that has days has had a link made under the terms, and a network
// with days but no link now (unlinked) qualifies once it links again.
func qualifyReferralReferees(ctx context.Context, now time.Time, terms *ReferralTermsSettings) (qualifiedCount int64, droppedCount int64) {
	today := auditDeviceDayStart(now)
	// the zero time is before every day
	var retentionStart time.Time
	if 0 < terms.AppUseRetention {
		retentionStart = today.Add(-terms.AppUseRetention)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		qualifiedCount, droppedCount = 0, 0
		if 0 < terms.AppUseDayCount {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_referral_referee (
						network_id,
						first_link_time,
						qualified_time
					)
					SELECT
						network_referral.network_id,
						network_referral.create_time,
						$3
					FROM (
						SELECT network_id
						FROM network_referral_app_day
						WHERE $1 <= app_byte_count
						GROUP BY network_id
						HAVING $2 <= count(*)
					) qualifying
					INNER JOIN network_referral ON
						network_referral.network_id = qualifying.network_id
					ON CONFLICT (network_id) DO UPDATE
					SET qualified_time = EXCLUDED.qualified_time
					WHERE network_referral_referee.qualified_time IS NULL
				`,
				terms.AppUseDailyByteCount,
				terms.AppUseDayCount,
				now,
			))
			qualifiedCount = tag.RowsAffected()
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_referral_app_day
				WHERE (network_id, day) IN (
					SELECT
						network_referral_app_day.network_id,
						network_referral_app_day.day
					FROM network_referral_app_day
					LEFT JOIN network_referral_referee ON
						network_referral_referee.network_id = network_referral_app_day.network_id
					WHERE
						network_referral_referee.qualified_time IS NOT NULL OR
						network_referral_app_day.day < $3::date OR (
							network_referral_app_day.day < $1::date AND
							network_referral_app_day.app_byte_count < $2
						)
					LIMIT $4
				)
			`,
			today,
			terms.AppUseDailyByteCount,
			retentionStart,
			max(terms.AppUseReapLimit, 0),
		))
		droppedCount = tag.RowsAffected()
	})
	return
}

// Adds a drained usage block's app use to the days of the referred networks
// that still have to qualify: the bytes each payer paid for, on the UTC day
// the block started, for a network whose link was made under the terms
// (existing_link false) and that has not qualified. A payer that is a hosted
// proxy, or descends from one, is not app use. The ancestry walk is bounded
// as session.ResolveClientRootInTx bounds it.
//
// It runs after the drain committed the block's usage, in its own
// transaction, and only for a block the drain applied, so a replayed block
// counts once. A failure is logged and loses only this block's app use, which
// can only delay a qualification; it never touches usage metering.
func recordReferralAppUse(ctx context.Context, blockStart time.Time, usage map[server.Id]ByteCount) {
	payerClientIds := make([]server.Id, 0, len(usage))
	byteCounts := make([]int64, 0, len(usage))
	for payerClientId, byteCount := range usage {
		payerClientIds = append(payerClientIds, payerClientId)
		byteCounts = append(byteCounts, byteCount)
	}
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					WITH RECURSIVE
					payer AS (
						SELECT
							usage.client_id,
							usage.byte_count,
							network_client.network_id
						FROM unnest($1::uuid[], $2::bigint[]) AS usage(client_id, byte_count)
						INNER JOIN network_client ON
							network_client.client_id = usage.client_id
						INNER JOIN network_referral ON
							network_referral.network_id = network_client.network_id AND
							NOT network_referral.existing_link
						LEFT JOIN network_referral_referee ON
							network_referral_referee.network_id = network_client.network_id
						WHERE network_referral_referee.qualified_time IS NULL
					),
					ancestry (payer_client_id, client_id, depth) AS (
						SELECT payer.client_id, payer.client_id, 1
						FROM payer
						UNION ALL
						SELECT
							ancestry.payer_client_id,
							network_client.source_client_id,
							ancestry.depth + 1
						FROM ancestry
						INNER JOIN network_client ON
							network_client.client_id = ancestry.client_id
						WHERE
							network_client.source_client_id IS NOT NULL AND
							ancestry.depth < $5
					)
					INSERT INTO network_referral_app_day (
						network_id,
						day,
						app_byte_count,
						update_time
					)
					SELECT
						payer.network_id,
						$3::date,
						sum(payer.byte_count),
						$4
					FROM payer
					WHERE NOT EXISTS (
						SELECT 1
						FROM ancestry
						INNER JOIN proxy_device_config ON
							proxy_device_config.client_id = ancestry.client_id
						WHERE ancestry.payer_client_id = payer.client_id
					)
					GROUP BY payer.network_id
					ON CONFLICT (network_id, day) DO UPDATE
					SET
						app_byte_count = network_referral_app_day.app_byte_count + EXCLUDED.app_byte_count,
						update_time = EXCLUDED.update_time
				`,
				payerClientIds,
				byteCounts,
				auditDeviceDayStart(blockStart),
				server.NowUtc(),
				session.ClientAncestryDepthLimit,
			))
		})
	}, func(err error) {
		glog.Errorf("[referral]the app use of the usage block at %s was not recorded: %s\n", blockStart.Format(time.RFC3339), err)
	})
}

// Marks the network as having qualified at qualifiedTime, as the grant would
// once its app use met the terms. For tests whose subject is not the app use.
func Testing_QualifyReferralReferee(ctx context.Context, networkId server.Id, qualifiedTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_referral_referee (
					network_id,
					first_link_time,
					qualified_time
				)
				VALUES (
					$1,
					COALESCE((SELECT create_time FROM network_referral WHERE network_id = $1), $2),
					$2
				)
				ON CONFLICT (network_id) DO UPDATE
				SET qualified_time = EXCLUDED.qualified_time
			`,
			networkId,
			qualifiedTime,
		))
	})
}
