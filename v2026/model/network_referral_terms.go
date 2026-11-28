// The referral terms past the bonus amounts (owner 2026-11-24): a referral
// made from now on must be claimed by the referred network's app use within a
// claim window, and once claimed it pays both sides for life. A link that
// existed when the terms began pays for life as before, and no grant already
// made is touched.
//
// Claiming. A link made after the terms began (network_referral.existing_link
// false, the column's default since migration 1010) pays neither side until
// its referred network qualifies: app use of at least AppUseDailyByteCount on
// at least AppUseDayCount distinct UTC days that start before the link's
// claim deadline, its create_time plus ClaimWindow. App use is the data the
// network's clients pay for, through any client but a hosted proxy (a client
// with a proxy_device_config), the windows it mints, and anything else
// descended from one. The usage rollup adds each drained block's app use to
// network_referral_app_day, by the UTC day the block started, for the
// networks that still have to claim (recordReferralAppUse). Connection
// history is kept for about a day, so the record starts with the terms and
// nothing before them counts. Each grant run first settles the claims
// (settleReferralClaims): it marks the networks whose days qualify them
// (network_referral_referee.qualified_time), so both sides earn from that run
// on, for life; it ends the links whose deadline passed unclaimed
// (network_referral.end_time); and it drops the days nothing can count any
// more. A qualified network stays qualified.
//
// Ending. An ended link stays as a record. It never pays, and like a link past
// its deadline unclaimed it no longer counts toward its referrer's
// max_referrals, so the slot is free for another referral
// (activeReferralCountInTx).
//
// The deadline counts from the network's first link: a re-point keeps the
// link's create_time, and a link made again after an unlink takes the time of
// the network's first link (network_referral_referee first_link_time), so
// neither restarts the window. A re-point to another referrer makes an
// existing link new, so it must be claimed within its original window, and a
// link made or re-pointed past its window unclaimed ends at once.
//
// Between the migration and the release that reads it, the running release
// still writes network_referral itself: its new links take the default and
// are new links, but a re-point it makes keeps existing_link and restarts
// create_time, and an unlink it makes records no first link. That window is
// the deploy's.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// pro.yml referral: an absent key keeps its default. The absent pro.yml
// (ProConfig{}) holds the zero value, which asks no app use, but no referral
// grant runs then.
type ReferralTermsSettings struct {
	// How long after its first link a new referral can be claimed (pro.yml
	// referral.claim_window). 90 days (owner 2026-11-24). Non-positive: no
	// deadline, so no link ends.
	ClaimWindow time.Duration
	// A qualifying day's app use (pro.yml referral.app_use_daily_bytes).
	// 10 MiB (owner 2026-11-24). Non-positive: any recorded app use.
	AppUseDailyByteCount ByteCount
	// How many distinct qualifying UTC days claim a referral (pro.yml
	// referral.app_use_days). 2 (owner 2026-11-24). Non-positive: none, so
	// every link pays at once and none ends.
	AppUseDayCount int
	// At most this many days of app use are dropped in one grant run; the
	// rest go in the next. Non-positive: none are dropped.
	AppUseReapLimit int
}

func DefaultReferralTermsSettings() ReferralTermsSettings {
	return ReferralTermsSettings{
		ClaimWindow:          90 * 24 * time.Hour,
		AppUseDailyByteCount: 10 * 1024 * 1024,
		AppUseDayCount:       2,
		AppUseReapLimit:      100000,
	}
}

// The defaults with pro.yml's keys applied. A malformed value panics, as
// every other pro.yml value does, rather than silently lift a limit.
func parseReferralTerms(y proReferralYaml) ReferralTermsSettings {
	terms := DefaultReferralTermsSettings()
	if y.ClaimWindow != "" {
		terms.ClaimWindow = mustParseDuration(y.ClaimWindow)
	}
	if y.AppUseDailyBytes != "" {
		terms.AppUseDailyByteCount = mustParseByteCount(y.AppUseDailyBytes)
	}
	if y.AppUseDays != nil {
		terms.AppUseDayCount = *y.AppUseDays
	}
	return terms
}

// Whether a new link can end: it must be claimed, and by a deadline.
func (self *ReferralTermsSettings) claimDeadlines() bool {
	return 0 < self.AppUseDayCount && 0 < self.ClaimWindow
}

// The claim window, in seconds, for the SQL that computes each link's
// deadline from its create_time.
func (self *ReferralTermsSettings) claimWindowSeconds() float64 {
	return self.ClaimWindow.Seconds()
}

// A link whose create_time is before this has passed its claim deadline at
// now. The zero time, before every link, when no link can end.
func (self *ReferralTermsSettings) claimCutoff(now time.Time) time.Time {
	if !self.claimDeadlines() {
		return time.Time{}
	}
	return now.Add(-self.ClaimWindow)
}

// One referral link, as the grant reads it.
type referralLink struct {
	networkId         server.Id
	referralNetworkId server.Id
	createTime        time.Time
	// the link existed when the terms began and was not re-pointed since
	existingLink bool
	// the referred network claimed it: its app use qualified it in time
	qualified bool
	// its claim deadline passed unclaimed
	ended bool
}

// Whether the link pays both its sides.
func referralLinkPays(link referralLink, terms *ReferralTermsSettings) bool {
	if link.existingLink {
		return true
	}
	if link.ended {
		return false
	}
	return terms.AppUseDayCount <= 0 || link.qualified
}

// referralNetworkId -> the networks it referred, by the links that pay.
func payingReferrals(links []referralLink, terms *ReferralTermsSettings) map[server.Id][]server.Id {
	referrals := map[server.Id][]server.Id{}
	for _, link := range links {
		if referralLinkPays(link, terms) {
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
					network_referral_referee.qualified_time IS NOT NULL,
					network_referral.end_time IS NOT NULL
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
					&link.ended,
				))
				links = append(links, link)
			}
		})
	})
	return links
}

// The referrer's links that count toward its max_referrals at now: every
// link but an ended one, or a new one past its claim deadline unclaimed.
const activeReferralLinkSql = `
	network_referral.end_time IS NULL AND (
		network_referral.existing_link OR
		$2 < network_referral.create_time OR
		EXISTS (
			SELECT 1 FROM network_referral_referee
			WHERE
				network_referral_referee.network_id = network_referral.network_id AND
				network_referral_referee.qualified_time IS NOT NULL
		)
	)
`

// How many of the referrer's links count toward its max_referrals at now.
func activeReferralCountInTx(ctx context.Context, tx server.PgTx, referralNetworkId server.Id, now time.Time) (count int) {
	terms := Pro().ReferralTerms
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM network_referral WHERE network_referral.referral_network_id = $1 AND `+activeReferralLinkSql,
		referralNetworkId,
		terms.claimCutoff(now),
	).Scan(&count))
	return
}

// Ends the network's link at once when it is new, unclaimed and already past
// its claim deadline: a re-point of an old link, or a link made again after
// an unlink. Inside the caller's transaction.
func endExpiredReferralLinkInTx(ctx context.Context, tx server.PgTx, networkId server.Id, now time.Time) {
	terms := Pro().ReferralTerms
	if !terms.claimDeadlines() {
		return
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_referral
			SET end_time = network_referral.create_time + make_interval(secs => $2::double precision)
			WHERE
				network_referral.network_id = $1 AND
				NOT network_referral.existing_link AND
				network_referral.end_time IS NULL AND
				network_referral.create_time + make_interval(secs => $2::double precision) <= $3 AND
				NOT EXISTS (
					SELECT 1 FROM network_referral_referee
					WHERE
						network_referral_referee.network_id = network_referral.network_id AND
						network_referral_referee.qualified_time IS NOT NULL
				)
		`,
		networkId,
		terms.claimWindowSeconds(),
		now,
	))
}

// Settles the claims at now, in one transaction:
//   - marks as qualified the referred networks of new, open links whose
//     qualifying days before the link's claim deadline meet the terms;
//   - ends the new links whose deadline passed unclaimed, at the deadline;
//   - drops the days of app use nothing can count any more: a qualified
//     network's, an ended link's, a past day short of a qualifying day, and a
//     day older than any open window.
//
// Days are UTC days, and now's day is still open. A network with days but no
// link (unlinked) can still claim when it links again within its window.
func settleReferralClaims(ctx context.Context, now time.Time, terms *ReferralTermsSettings) (claimedCount int64, endedCount int64, droppedCount int64) {
	today := auditDeviceDayStart(now)
	// the zero time is before every day
	var retentionStart time.Time
	if terms.claimDeadlines() {
		retentionStart = today.Add(-terms.ClaimWindow - 24*time.Hour)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		claimedCount, endedCount, droppedCount = 0, 0, 0
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
						$4
					FROM network_referral
					INNER JOIN network_referral_app_day ON
						network_referral_app_day.network_id = network_referral.network_id
					WHERE
						NOT network_referral.existing_link AND
						network_referral.end_time IS NULL AND
						$1 <= network_referral_app_day.app_byte_count AND (
							$3::double precision <= 0 OR
							network_referral_app_day.day < network_referral.create_time + make_interval(secs => $3::double precision)
						)
					GROUP BY network_referral.network_id, network_referral.create_time
					HAVING $2 <= count(*)
					ON CONFLICT (network_id) DO UPDATE
					SET qualified_time = EXCLUDED.qualified_time
					WHERE network_referral_referee.qualified_time IS NULL
				`,
				terms.AppUseDailyByteCount,
				terms.AppUseDayCount,
				terms.claimWindowSeconds(),
				now,
			))
			claimedCount = tag.RowsAffected()
		}
		if terms.claimDeadlines() {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_referral
					SET end_time = network_referral.create_time + make_interval(secs => $2::double precision)
					WHERE
						NOT network_referral.existing_link AND
						network_referral.end_time IS NULL AND
						network_referral.create_time + make_interval(secs => $2::double precision) <= $1 AND
						NOT EXISTS (
							SELECT 1 FROM network_referral_referee
							WHERE
								network_referral_referee.network_id = network_referral.network_id AND
								network_referral_referee.qualified_time IS NOT NULL
						)
				`,
				now,
				terms.claimWindowSeconds(),
			))
			endedCount = tag.RowsAffected()
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
					LEFT JOIN network_referral ON
						network_referral.network_id = network_referral_app_day.network_id
					WHERE
						network_referral_referee.qualified_time IS NOT NULL OR
						network_referral.end_time IS NOT NULL OR
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
// that still have to claim: the bytes each payer paid for, on the UTC day the
// block started, for a network whose link was made under the terms
// (existing_link false), is open and has not been claimed. A payer that is a
// hosted proxy, or descends from one, is not app use. The ancestry walk is
// bounded as session.ResolveClientRootInTx bounds it.
//
// It runs after the drain committed the block's usage, in its own
// transaction, and only for a block the drain applied, so a replayed block
// counts once. A failure is logged and loses only this block's app use, which
// can only delay a claim; it never touches usage metering.
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
							NOT network_referral.existing_link AND
							network_referral.end_time IS NULL
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
// once its app use claimed its link. For tests whose subject is not the app
// use.
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
