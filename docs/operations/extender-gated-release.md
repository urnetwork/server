# Gated extender release: checks before enabling gated preference

Written 2026-11-02 with the server half of connect/EXTENDER-UPDATE2.md §1.
Nothing here changes production. It is the read-only evidence the rollout
gates ask for before clients prefer gated records: that the release has a
fleet to draw from, and that the blocked state's inputs are recorded.

## How the blocked state decides

A record is skipped in a country when both hold:

- At least 3 distinct clients there reported it within 24 hours
  (`POST /network/extender-block-report`).
- The operator's probe (the taskworker's extender probe, every 5 minutes)
  reaches it now: every active address's latest probe succeeded within the
  last hour.

A report sent while the probe fails is not recorded. A failed probe deletes
the extender's reports made before it, in every country, since they describe
the outage rather than a block. The task result counts them
(`discounted_block_reports`), and so does the probe's summary log line.

The probe's log lines name an extender by its id and family, never its address,
since a gated address must not reach a log. To find the address of an extender a
line names, read `network_extender_address` by that id.

## What a client reports

The sender is in the SDK (`extender_block_report.go`), and the rule of what counts
is in connect (`net_extender_directory_gated_block.go`). A client reports a record,
by its public key alone, only when all of these hold:

- A real dial of its verified record failed. That dial alone counts for nothing: it
  only makes the record worth checking. The record is one the client was released,
  or one its connection path drew, and it is current, neither revoked nor expired.
- Three attempts of the client's own failed, at least 30 minutes apart, so at least
  an hour from first to last. An attempt dials every carrier the record offers on
  every address it names in the families the client dials, 15 seconds a dial, the
  same budget as the operator's probe.
- The control route answered before each attempt, or nothing is dialed, and after
  it, or it does not count. A client with no network reports nothing.
- All of it on one network path. A path change drops everything and starts over.
- Nothing answered meanwhile. A success or a 429 from any address on any carrier,
  by an attempt or any other dial, drops the record.

**The carrier question.** A report names no carrier, and the operator's probe dials
tcp only. The wire has no carrier field, and needs none: a report goes only after
every carrier the record offers failed, tcp included, and a record with no tcp
carrier on an address of the client's families is never reported. So a report never
means one carrier failed. It means the record as a whole did not answer, while the
same tcp dial from outside did. A client on a network that blocks only udp, say,
never reports a record whose tcp answers.

**Bounds.** One loop per network space does one thing at a time.

- At most one attempt pass every 30 minutes, of at most 3 records. Gated records
  come first, then the oldest.
- A control route that does not answer holds the next pass, from 5 minutes doubling
  to an hour.
- At most 4 reports an hour per identity, 60 seconds apart. A report with no answer
  backs off from 5 minutes to 30 minutes, and a refusal waits out the hour.
- A record reported is left alone for a day, and a record that answered for 6 hours.
- A client with nothing failing makes no extra dial or request. Nothing is stored, so
  a restart starts over.

**Controls.** The sender runs under the same cohort control as gated preference
(`NetworkSpaceValues.ExtenderGatedPreference`), and is off by default. A hosted space
never reports. An account change cancels what is in flight and discards it. A report
changes nothing in what the client trusts or dials. The client logs no address or
key, and counts over fixed labels only.

Read reports as reachability evidence: they say the record did not answer these
clients while the probe reached it. The operator's rules decide from there, as
above. The server counts distinct clients, not networks, so three devices behind one
firewall can reach the threshold.

A jump in `pairs_at_threshold_1d` across most gated records of a country at once
points at a client build or a network there, not at a censor that found each record.
Check what rolled out before acting on it. Turning the cohort off stops new reports,
and the ones already recorded age out of the 24-hour window.

## The check

Run on main as a read-only battery. Every count leaves the database as a
bucket or a coarse share, and no row names an extender, address, key, client,
network or country. Use the usual read-only wrapper that reads the password
from stdin.

```sql
BEGIN READ ONLY;
SET LOCAL statement_timeout = '15s';
SET LOCAL lock_timeout = '1s';

-- reports, releases and probes, bucketed
WITH raw(metric, n) AS (
	SELECT * FROM (VALUES
		('reports_all', (SELECT COUNT(*) FROM network_extender_block_report)),
		('reports_1d', (SELECT COUNT(*) FROM network_extender_block_report WHERE (now() AT TIME ZONE 'utc') - interval '1 day' <= report_time)),
		('pairs_at_threshold_1d', (
			SELECT COUNT(*) FROM (
				SELECT 1 FROM network_extender_block_report
				WHERE (now() AT TIME ZONE 'utc') - interval '1 day' <= report_time
				GROUP BY extender_id, country_code
				HAVING 3 <= COUNT(DISTINCT client_id)
			) AS pairs
		)),
		('release_requests_all', (SELECT COUNT(*) FROM network_extender_release_request)),
		('releases_all', (SELECT COUNT(*) FROM network_extender_release)),
		('gated_active_addresses', (
			SELECT COUNT(*) FROM network_extender_address AS a
			INNER JOIN network_extender AS e ON e.extender_id = a.extender_id AND e.active
			WHERE a.active AND e.directory_tier = 1
		))
	) AS v(metric, n)
)
SELECT metric, CASE
	WHEN n = 0 THEN '0'
	WHEN n < 10 THEN '1-9'
	WHEN n < 100 THEN '10-99'
	WHEN n < 1000 THEN '100-999'
	ELSE '>=1000'
END AS bucket
FROM raw;

-- the probe of active addresses by tier and family, as shares
WITH grouped AS (
	SELECT
		CASE WHEN e.directory_tier = 0 THEN 'open' ELSE 'gated' END AS tier,
		a.ip_version AS family,
		COUNT(*) AS whole,
		COUNT(*) FILTER (WHERE (now() AT TIME ZONE 'utc') - interval '1 hour' <= a.last_probe_time) AS probed_1h,
		COUNT(*) FILTER (WHERE a.consecutive_probe_failures = 0 AND (now() AT TIME ZONE 'utc') - interval '1 hour' <= a.last_probe_success_time) AS reached_now
	FROM network_extender_address AS a
	INNER JOIN network_extender AS e ON e.extender_id = a.extender_id AND e.active
	WHERE a.active
	GROUP BY 1, 2
)
SELECT tier, family, metric, CASE
	WHEN part = 0 THEN '0%'
	WHEN part = whole THEN '100%'
	WHEN part * 100 < whole * 50 THEN '<50%'
	WHEN part * 100 < whole * 90 THEN '50-90%'
	ELSE '90-100%'
END AS share
FROM grouped
CROSS JOIN LATERAL (VALUES ('probed_1h', probed_1h), ('reached_now', reached_now)) AS v(metric, part)
ORDER BY tier, family, metric;

ROLLBACK;
```

What the gate needs:

- `gated_active_addresses` is not `0` in both families the rollout cohort
  dials, and the fleet is sized for the cohort (EXTENDER-UPDATE2 §1.2: at
  most 10 identities per record per country over 30 days).
- `probed_1h` is `100%` for the gated tier in both families. A family whose
  `reached_now` stays low (for example IPv6 on a prober without it) keeps
  every dual-family record on it from ever being judged blocked.
- `reports_all` is not `0` once clients send block reports. Until then the
  blocked state never blocks anything, and a blocked record is still
  released.

## Status on 2026-11-02

From the battery above, on main:

- Block reports: none recorded within the 31-day retention. No client sends
  them: neither connect nor the SDK calls the route.
- Releases: no request and no release recorded. No deployed client calls the
  route. The SDK's fetch (slice A) reached sdk main on 2026-11-02, and it asks
  only for a space a cohort has turned gated preference on for.
- Gated fleet: none. Every active extender address is in the open tier, so a
  release answers `empty` for every identity.
- Probe: running. Every active address in both families was probed within
  the hour, the newest within 10 minutes. 90-100% of IPv4 and 50-90% of IPv6
  addresses are reached now.

So the probe half of the blocked state's input is recorded, and the report
half is not. Before gated preference is enabled for a cohort, a client must
send block reports, and the operator must move hosts into the gated tier.

Later on 2026-11-02 the SDK's block report sender reached sdk main, under the same
cohort control, so a build with it sends reports as soon as a cohort is turned on.
`reports_all` stays `0` until then. After a cohort is on, a sustained `0` with gated
addresses failing is a reason to look at the sender.
