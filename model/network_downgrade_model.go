package model

// §20.7's On downgrade column (QUOTA-M6-05; D-QUOTA-6, FS-51, R-QUOTA-5): nothing deleted, no inbound address
// broken, growth frozen. ApplyNetworkDowngrades is the daily job, one bounded page of networks a call, resumable
// by its network cursor. QUOTA owns the clock, NetworkDowngradeTime (team_balance_model.go); DNS and TLS own the
// effects (dns_withhold_model.go, tls_suspension_model.go), and this file only decides when each is called.
//
// Which networks. Only a network the effects could touch is read (R-QUOTA-5): one with user RRsets, a custom
// host, a policy document, a downgrade's kill, a row over its storage limit, or stored bytes and a tier window
// that ended within LapseLookback. Each is read from the source of truth, never the tier cache.
//
// What a pass does to one network, by where it stands on the clock:
//
//	release   it has left no tier: it never did, or a renewal put it back. What a downgrade withheld is served
//	          again, oldest first, and nothing more is withheld here
//	hold      it left a tier less than DowngradeGrace ago: nothing changes, so day 13 is as day 0, and a renewal
//	          inside the 14 days finds nothing to undo
//	converge  it left a tier DowngradeGrace ago or longer: its names are brought to its tier's limits now, which
//	          withholds what is over them and serves again what a later, higher tier allows
//
// Storage follows no clock: every pass recomputes storage_over from the stored count and the tier's limit now, so
// storage goes read-only at the first pass after the tier drops and opens at the first after a renewal. The usage
// batches' deltas and the nightly reconcile recompute it too, through the tier cache.
//
// The effects of a converge:
//
//   - dns_entries: the newest user RRsets past the limit are withheld through the journal with their rows kept
//     (DnsWithholdOverQuotaInTx). The limit counts a network's RRsets in every root, so each root is given the
//     share of the limit its oldest RRsets take, and the newest go whichever root holds them.
//   - custom domains: Team only, so below team each active web host goes withheld (DnsWithholdCustomHostsInTx),
//     and on team each comes back (DnsReleaseCustomHostsInTx). MX, TLSA and routing are never touched. A custom
//     domain's mail From is smtpedge's From-domain gate, which this job does not drive.
//   - tls_endpoints: the network's published (host, port) names are counted as its projection lists them
//     (TlsPublishedNamesSource), and while they pass the limit, hosts that hold a standing name, one with no
//     expiry, are killed with reason quota:downgrade, last in host order first, until the rest fit. A grant with
//     an expiry is never killed: it expires. The kill is §13.9's row and key, written only where no other kill
//     holds the host and lifted only while it is still this job's, so an abuse kill is never overwritten or
//     lifted here (RESOLUTIONS #17: no §21 action, not mailed, re-derived by the next pass after a restore). A
//     host no kill can name alone, a glob, is left: this job never kills every host of a network.
//
// dns_entries and tls_endpoints are limits, which ship dark: while enforce_sidecar_limits is off they read as no
// limit, so nothing is withheld for them and what was is served again. The Team tier's own gate is never dark, so
// custom hosts follow the tier whatever the switch says.
//
// Every effect converges, so a rerun writes nothing and a killed pass is finished by the next: each root's zone,
// the custom hosts and each host's kill commit on their own. A zone and the custom hosts take separate
// transactions, since each takes the admin event chain's lock and a custom-host change raises its map first.
// Existing admissions and ACL heads, smtp_* and connected clients need no job: their checks read the tier at the
// next creation, preamble or connect (§20.7), and §7.5's routes freeze by NetworkTierOf.
//
// A binary that does not link MG2, MG5 and MG6 does nothing here.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The reason of a kill a downgrade set (§20.7's tls_endpoints row): what tells it from an abuse case's.
const TlsSuspensionReasonQuotaDowngrade = "quota:downgrade"

// The system publish's host, tls/443/mta-sts (§13.9, §15.5): it takes no tls_endpoints unit, and no downgrade
// kills it, since a receiver's MTA-STS policy answers there.
const (
	tlsDowngradeSystemHost = "mta-sts"
	tlsDowngradeSystemPort = 443
)

// The job's effects, as urnetwork_quota_downgrade_total names them.
const (
	networkDowngradeEffectDnsWithheld         = "dns_withheld"
	networkDowngradeEffectDnsReleased         = "dns_released"
	networkDowngradeEffectCustomHostsWithheld = "custom_hosts_withheld"
	networkDowngradeEffectCustomHostsReleased = "custom_hosts_released"
	networkDowngradeEffectTlsSuspended        = "tls_suspended"
	networkDowngradeEffectTlsLifted           = "tls_lifted"
	networkDowngradeEffectStorageOver         = "storage_over"
	networkDowngradeEffectError               = "error"
)

var networkDowngradeCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_quota_downgrade_total",
	Help: "What the daily downgrade job changed, by effect: zones written, custom hosts and kills moved, storage_over corrected, and networks a pass could not finish.",
}, []string{"effect"})

func init() {
	prometheus.MustRegister(networkDowngradeCounter)
}

// The published names of a network could not be read, so its kills were left as they stood.
var ErrNetworkDowngradeNamesUnavailable = errors.New("network downgrade: the network's published names are unavailable")

type NetworkDowngradeSettings struct {
	// §20.7's 14 days from NetworkDowngradeTime before anything is withheld. The TLS listen facts' standing
	// grants read the same value (DefaultTlsListenFactsSettings).
	DowngradeGrace time.Duration
	// Networks one call takes.
	PageCount int
	// How long after a tier window's end a network with stored bytes is still read for storage_over: three
	// passes of a daily job, so one missed run loses none.
	LapseLookback time.Duration
}

func DefaultNetworkDowngradeSettings() *NetworkDowngradeSettings {
	return &NetworkDowngradeSettings{
		DowngradeGrace: 14 * 24 * time.Hour,
		PageCount:      200,
		LapseLookback:  3 * 24 * time.Hour,
	}
}

// One page of the job.
type ApplyNetworkDowngradesResult struct {
	LookedCount int
	// zones a pass wrote, by what the write did
	DnsWithheldCount int
	DnsReleasedCount int
	// custom web hosts moved
	CustomHostsWithheldCount int
	CustomHostsReleasedCount int
	// hosts killed and kills lifted
	TlsSuspendedCount int
	TlsLiftedCount    int
	// rows whose storage_over was wrong
	StorageOverCorrected int
	// networks a pass could not finish, which the next pass takes again
	ErrorCount int
	// the page's last network, where the next page starts; nil when the pass is done
	LastNetworkId *server.Id
}

func (self *ApplyNetworkDowngradesResult) add(other *ApplyNetworkDowngradesResult) {
	self.DnsWithheldCount += other.DnsWithheldCount
	self.DnsReleasedCount += other.DnsReleasedCount
	self.CustomHostsWithheldCount += other.CustomHostsWithheldCount
	self.CustomHostsReleasedCount += other.CustomHostsReleasedCount
	self.TlsSuspendedCount += other.TlsSuspendedCount
	self.TlsLiftedCount += other.TlsLiftedCount
	self.StorageOverCorrected += other.StorageOverCorrected
}

// Where a network stands on the downgrade clock (see the file header).
type networkDowngradeMode int

const (
	networkDowngradeModeRelease networkDowngradeMode = iota
	networkDowngradeModeHold
	networkDowngradeModeConverge
)

func networkDowngradeLinked() bool {
	return server.SidecarMg2Linked() && server.SidecarMg5Linked() && server.SidecarMg6Linked()
}

// One page of at most PageCount networks after after (nil: the start), at now: see the file header. A network
// the pass could not finish is counted and logged, and the page goes on, so one network's failure holds no
// other's release.
func ApplyNetworkDowngrades(ctx context.Context, now time.Time, after *server.Id) (ApplyNetworkDowngradesResult, error) {
	return ApplyNetworkDowngradesWithSettings(ctx, now, after, DefaultNetworkDowngradeSettings())
}

func ApplyNetworkDowngradesWithSettings(ctx context.Context, now time.Time, after *server.Id, settings *NetworkDowngradeSettings) (result ApplyNetworkDowngradesResult, returnErr error) {
	if !networkDowngradeLinked() {
		return result, nil
	}
	if settings.PageCount <= 0 {
		return result, errors.New("a downgrade page of no networks")
	}
	networkIds := networkDowngradeCandidates(ctx, now, after, settings)
	result.LookedCount = len(networkIds)
	if len(networkIds) == settings.PageCount {
		lastNetworkId := networkIds[len(networkIds)-1]
		result.LastNetworkId = &lastNetworkId
	}
	for _, networkId := range networkIds {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		networkResult, err := applyNetworkDowngrade(ctx, networkId, now, settings)
		result.add(networkResult)
		if err != nil {
			result.ErrorCount += 1
			glog.Errorf("[quota][downgrade]%s not finished: %s\n", networkId, err)
		}
	}
	for effect, count := range map[string]int{
		networkDowngradeEffectDnsWithheld:         result.DnsWithheldCount,
		networkDowngradeEffectDnsReleased:         result.DnsReleasedCount,
		networkDowngradeEffectCustomHostsWithheld: result.CustomHostsWithheldCount,
		networkDowngradeEffectCustomHostsReleased: result.CustomHostsReleasedCount,
		networkDowngradeEffectTlsSuspended:        result.TlsSuspendedCount,
		networkDowngradeEffectTlsLifted:           result.TlsLiftedCount,
		networkDowngradeEffectStorageOver:         result.StorageOverCorrected,
		networkDowngradeEffectError:               result.ErrorCount,
	} {
		if 0 < count {
			networkDowngradeCounter.WithLabelValues(effect).Add(float64(count))
		}
	}
	return result, nil
}

// The next page of networks an effect could touch, in network order (see the file header). Each arm is bounded
// by the page on its own index, so the statement reads at most six pages.
func networkDowngradeCandidates(ctx context.Context, now time.Time, after *server.Id, settings *NetworkDowngradeSettings) (networkIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id FROM (
					(
						SELECT DISTINCT network_id
						FROM network_dns_record
						WHERE network_id IS NOT NULL AND kind = $3 AND ($1::uuid IS NULL OR $1 < network_id)
						ORDER BY network_id
						LIMIT $2
					)
					UNION
					(
						SELECT DISTINCT network_id
						FROM network_dns_custom_host
						WHERE $1::uuid IS NULL OR $1 < network_id
						ORDER BY network_id
						LIMIT $2
					)
					UNION
					(
						SELECT DISTINCT network_id
						FROM network_tls_suspension
						WHERE reason = $4 AND ($1::uuid IS NULL OR $1 < network_id)
						ORDER BY network_id
						LIMIT $2
					)
					UNION
					(
						SELECT network_id
						FROM network_acl_head
						WHERE has_document AND ($1::uuid IS NULL OR $1 < network_id)
						ORDER BY network_id
						LIMIT $2
					)
					UNION
					(
						SELECT network_id
						FROM network_quota_state
						WHERE storage_over AND ($1::uuid IS NULL OR $1 < network_id)
						ORDER BY network_id
						LIMIT $2
					)
					UNION
					(
						SELECT DISTINCT transfer_balance.network_id
						FROM transfer_balance
						INNER JOIN network_quota_state ON network_quota_state.network_id = transfer_balance.network_id
						WHERE
							transfer_balance.pro = true AND
							$5 < transfer_balance.end_time AND
							transfer_balance.end_time <= $6 AND
							($1::uuid IS NULL OR $1 < transfer_balance.network_id)
						ORDER BY transfer_balance.network_id
						LIMIT $2
					)
				) candidate
				ORDER BY network_id
				LIMIT $2
			`,
			after,
			settings.PageCount,
			string(dnsservice.DnsRecordKindUser),
			TlsSuspensionReasonQuotaDowngrade,
			now.UTC().Add(-settings.LapseLookback),
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	return
}

// The network's limits at tier after its network_sidecar_limit rows, as NetworkSidecarLimits reads them, for a
// tier read from the source of truth.
func networkSidecarLimitsOfTier(ctx context.Context, networkId server.Id, tier NetworkTier) *ProSidecarLimits {
	limits := Pro().TierFor(tier).Sidecar
	for limitKey, limitValue := range networkSidecarLimitSnapshotLocal.keyValues(ctx, nil, networkId) {
		limits = limits.withLimit(limitKey, limitValue)
	}
	return &limits
}

// A count limit as the job applies it: none while enforce_sidecar_limits is off (see the file header).
func networkDowngradeLimit(limits *ProSidecarLimits, limitKey string) int64 {
	if !Pro().EnforceSidecarLimits {
		return proSidecarLimitNone
	}
	return limits.Limit(limitKey)
}

// One network's pass. Each part runs whatever another's error, and the errors are joined.
func applyNetworkDowngrade(ctx context.Context, networkId server.Id, now time.Time, settings *NetworkDowngradeSettings) (result *ApplyNetworkDowngradesResult, returnErr error) {
	result = &ApplyNetworkDowngradesResult{}
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = errors.Join(returnErr, err)
			} else {
				returnErr = errors.Join(returnErr, fmt.Errorf("network downgrade: %v", r))
			}
		}
	}()
	_, tierEntitlement := loadNetworkTierInConn(nil, ctx, networkId)
	tier := tierEntitlement.tier
	mode := networkDowngradeModeRelease
	if downgradeTime, _, left := NetworkDowngradeTime(ctx, networkId); left {
		mode = networkDowngradeModeConverge
		if now.Before(downgradeTime.Add(settings.DowngradeGrace)) {
			mode = networkDowngradeModeHold
		}
	}
	limits := networkSidecarLimitsOfTier(ctx, networkId, tier)

	// storage follows no clock
	result.StorageOverCorrected = recomputeNetworkStorageOver(ctx, networkId, networkStorageLimitByteCount(tier, limits, &Pro().Team))
	if mode == networkDowngradeModeHold {
		return result, nil
	}

	errs := []error{}
	if err := applyNetworkDowngradeDns(ctx, networkId, mode, networkDowngradeLimit(limits, DnsEntriesLimitKey), result); err != nil {
		errs = append(errs, err)
	}
	if err := applyNetworkDowngradeCustomHosts(ctx, networkId, mode, tier, result); err != nil {
		errs = append(errs, err)
	}
	if err := applyNetworkDowngradeTls(ctx, networkId, mode, networkDowngradeLimit(limits, sidecar.AclTlsEndpointsLimitKey), now, result); err != nil {
		errs = append(errs, err)
	}
	return result, errors.Join(errs...)
}

// Sets the network's storage_over to what its stored count and limitByteCount give, where its row says
// otherwise. Answers how many rows changed: one or none.
func recomputeNetworkStorageOver(ctx context.Context, networkId server.Id, limitByteCount int64) (correctedCount int) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_quota_state
				SET storage_over = ($2::bigint <> $3 AND $2 <= stored_byte_count)
				WHERE
					network_id = $1 AND
					storage_over <> ($2::bigint <> $3 AND $2 <= stored_byte_count)
			`,
			networkId,
			limitByteCount,
			proSidecarLimitNone,
		))
		correctedCount = int(tag.RowsAffected())
	})
	return
}

// One root's share of a network's dns_entries.
type networkDowngradeDnsRoot struct {
	root string
	// its user RRsets, those not withheld, and those among the network's oldest limit
	totalCount  int64
	servedCount int64
	keepCount   int64
	// whether a row stands on the wrong side of the root's target
	changed bool
}

// The network's roots with user RRsets, each with the most of its RRsets to serve under limit, -1 for none: the
// network's oldest limit RRsets by first write stay, in whichever root, so each root keeps its own oldest
// keepCount, which is the order DnsWithholdOverQuotaInTx withholds in. With release, a root serves no fewer
// than it does now, so the call withholds nothing more.
func networkDowngradeDnsRoots(ctx context.Context, networkId server.Id, limit int64, release bool) []*networkDowngradeDnsRoot {
	type userRow struct {
		root     string
		withheld bool
	}
	rows := []userRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT root, withheld_time IS NOT NULL
				FROM network_dns_record
				WHERE network_id = $1 AND kind = $2
				ORDER BY create_time, root, owner, rrtype
			`,
			networkId,
			string(dnsservice.DnsRecordKindUser),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := userRow{}
				server.Raise(result.Scan(&row.root, &row.withheld))
				rows = append(rows, row)
			}
		})
	})
	rootStates := map[string]*networkDowngradeDnsRoot{}
	roots := []*networkDowngradeDnsRoot{}
	for index, row := range rows {
		state, ok := rootStates[row.root]
		if !ok {
			state = &networkDowngradeDnsRoot{root: row.root}
			rootStates[row.root] = state
			roots = append(roots, state)
		}
		state.totalCount += 1
		if !row.withheld {
			state.servedCount += 1
		}
		if limit < 0 || int64(index) < limit {
			state.keepCount += 1
		}
	}
	if release {
		for _, state := range roots {
			state.keepCount = max(state.keepCount, state.servedCount)
		}
	}
	// a root's rows come in its own order, so its first keepCount are the ones to serve
	rootIndexes := map[string]int64{}
	for _, row := range rows {
		state := rootStates[row.root]
		served := rootIndexes[row.root] < state.keepCount
		rootIndexes[row.root] += 1
		if served == row.withheld {
			state.changed = true
		}
	}
	slices.SortFunc(roots, func(a *networkDowngradeDnsRoot, b *networkDowngradeDnsRoot) int {
		return cmp.Compare(a.root, b.root)
	})
	return roots
}

// dns_entries' effect: each root's zone brought to its share of limit, one transaction a root.
func applyNetworkDowngradeDns(ctx context.Context, networkId server.Id, mode networkDowngradeMode, limit int64, result *ApplyNetworkDowngradesResult) error {
	release := mode == networkDowngradeModeRelease
	errs := []error{}
	for _, root := range networkDowngradeDnsRoots(ctx, networkId, limit, release) {
		if !root.changed {
			continue
		}
		var write *DnsWriteResult
		var err error
		server.Tx(ctx, func(tx server.PgTx) {
			if release {
				write, err = DnsReleaseWithheldInTx(ctx, tx, networkId, root.root, root.keepCount, false)
			} else {
				write, err = DnsWithholdOverQuotaInTx(ctx, tx, networkId, root.root, root.keepCount)
			}
		}, server.TxReadCommitted)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if write == nil {
			// a suspension holds the rows, or another write settled them first
			continue
		}
		if root.keepCount < root.servedCount {
			result.DnsWithheldCount += 1
		} else {
			result.DnsReleasedCount += 1
		}
	}
	return errors.Join(errs...)
}

// Whether the network has a custom host row in state.
func networkHasDnsCustomHostInState(ctx context.Context, networkId server.Id, state string) (exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM network_dns_custom_host WHERE network_id = $1 AND state = $2)`,
			networkId,
			state,
		).Scan(&exists))
	})
	return
}

// Custom domains' effect, Team only: on team the withheld web hosts come back; below team, once the 14 days are
// past, the active ones go withheld. A network below team that has left no tier is left alone.
func applyNetworkDowngradeCustomHosts(ctx context.Context, networkId server.Id, mode networkDowngradeMode, tier NetworkTier, result *ApplyNetworkDowngradesResult) error {
	switch {
	case tier == NetworkTierTeam:
		if !networkHasDnsCustomHostInState(ctx, networkId, DnsCustomHostStateWithheld) {
			return nil
		}
		var count int
		var err error
		server.Tx(ctx, func(tx server.PgTx) {
			count, err = DnsReleaseCustomHostsInTx(ctx, tx, networkId)
		}, server.TxReadCommitted)
		if err != nil {
			return err
		}
		result.CustomHostsReleasedCount += count
	case mode == networkDowngradeModeConverge:
		if !networkHasDnsCustomHostInState(ctx, networkId, sidecar.DnsCustomHostStateActive) {
			return nil
		}
		var count int
		var err error
		server.Tx(ctx, func(tx server.PgTx) {
			count, err = DnsWithholdCustomHostsInTx(ctx, tx, networkId)
		}, server.TxReadCommitted)
		if err != nil {
			return err
		}
		result.CustomHostsWithheldCount += count
	}
	return nil
}

// The hosts of the network a downgrade's kill holds now, sorted.
func getTlsDowngradeSuspendedHosts(ctx context.Context, networkId server.Id) (hosts []string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT host
				FROM network_tls_suspension
				WHERE network_id = $1 AND reason = $2
				ORDER BY host
			`,
			networkId,
			TlsSuspensionReasonQuotaDowngrade,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var host string
				server.Raise(result.Scan(&host))
				hosts = append(hosts, host)
			}
		})
	})
	return
}

// Whether the network's log holds a policy document, without which no rule publishes a standing name.
func networkHasAclDocument(ctx context.Context, networkId server.Id) (hasDocument bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM network_acl_head WHERE network_id = $1 AND has_document)`,
			networkId,
		).Scan(&hasDocument))
	})
	return
}

// The hosts a downgrade kills for names over limit (see the file header): none while the (host, port) names fit.
// Past the limit, a host with a standing name goes, last in host order first, until the names of the hosts left
// fit; a host whose names all expire, the system publish's, and one no kill names alone are never taken, so the
// rest may still pass the limit.
func tlsDowngradeSuspendHosts(names []*TlsPublishedName, limit int64, settings *TlsSuspensionSettings) []string {
	if limit < 0 {
		return nil
	}
	type endpoint struct {
		host string
		port int
	}
	endpoints := map[endpoint]bool{}
	hostEndpointCounts := map[string]int64{}
	hostStanding := map[string]bool{}
	for _, name := range names {
		if name == nil || (name.Host == tlsDowngradeSystemHost && name.Port == tlsDowngradeSystemPort) {
			continue
		}
		// one grant is listed under each name it is served by
		key := endpoint{host: name.Host, port: name.Port}
		if !endpoints[key] {
			endpoints[key] = true
			hostEndpointCounts[name.Host] += 1
		}
		if name.ExpireTime.IsZero() {
			hostStanding[name.Host] = true
		}
	}
	if int64(len(endpoints)) <= limit {
		return nil
	}
	keptCount := int64(0)
	candidates := []string{}
	for host, count := range hostEndpointCounts {
		suspendable := hostStanding[host] &&
			host != tlsDowngradeSystemHost &&
			host != sidecar.TlsKillAllHosts &&
			tlsSuspensionHostValid(host, settings)
		if suspendable {
			candidates = append(candidates, host)
		} else {
			keptCount += count
		}
	}
	slices.Sort(candidates)
	suspendHosts := []string{}
	for _, host := range candidates {
		if keptCount+hostEndpointCounts[host] <= limit {
			keptCount += hostEndpointCounts[host]
		} else {
			suspendHosts = append(suspendHosts, host)
		}
	}
	return suspendHosts
}

// tls_endpoints' effect: the downgrade's kills brought to the hosts over limit, each host on its own.
func applyNetworkDowngradeTls(ctx context.Context, networkId server.Id, mode networkDowngradeMode, limit int64, now time.Time, result *ApplyNetworkDowngradesResult) error {
	suspendedHosts := getTlsDowngradeSuspendedHosts(ctx, networkId)
	suspendHosts := []string{}
	if mode == networkDowngradeModeConverge && 0 <= limit && networkHasAclDocument(ctx, networkId) {
		source := tlsPublishedNamesSource.Load()
		if source == nil {
			// kills are neither set nor lifted on names that cannot be read
			return ErrNetworkDowngradeNamesUnavailable
		}
		names, err := (*source)(ctx, networkId, now)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrNetworkDowngradeNamesUnavailable, err)
		}
		suspendHosts = tlsDowngradeSuspendHosts(names, limit, DefaultTlsSuspensionSettings())
	}
	errs := []error{}
	for _, host := range suspendHosts {
		set, err := setTlsDowngradeSuspension(ctx, networkId, host)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if set {
			result.TlsSuspendedCount += 1
		}
	}
	for _, host := range suspendedHosts {
		if slices.Contains(suspendHosts, host) {
			continue
		}
		lifted, err := liftTlsDowngradeSuspension(ctx, networkId, host)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if lifted {
			result.TlsLiftedCount += 1
		}
	}
	return errors.Join(errs...)
}

// Kills host for the downgrade, as SetTlsSuspension writes a kill, the row then the key and its trigger, but
// only where no kill holds the host: a row of another reason, an abuse case's, stays as it is and is that case's
// to lift. A row of this reason whose key a killed pass never wrote gets its key. Answers whether the row was
// written now.
func setTlsDowngradeSuspension(ctx context.Context, networkId server.Id, host string) (set bool, returnErr error) {
	value, err := tlsSuspensionEntryValue(host, TlsSuspensionReasonQuotaDowngrade, time.Time{})
	if err != nil {
		return false, err
	}
	reason := ""
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_tls_suspension (network_id, host, reason, until_time)
				VALUES ($1, $2, $3, NULL)
				ON CONFLICT (network_id, host) DO NOTHING
			`,
			networkId,
			host,
			TlsSuspensionReasonQuotaDowngrade,
		))
		set = tag.RowsAffected() == 1
		reason = ""
		result, err := tx.Query(
			ctx,
			`SELECT reason FROM network_tls_suspension WHERE network_id = $1 AND host = $2`,
			networkId,
			host,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&reason))
			}
		})
	})
	if reason != TlsSuspensionReasonQuotaDowngrade {
		return false, nil
	}
	if !set {
		held := ""
		server.Redis(ctx, func(r server.RedisClient) {
			held, returnErr = r.HGet(ctx, sidecar.TlsKillKey(sidecar.Id(networkId)), host).Result()
			if errors.Is(returnErr, server.RedisNil) {
				returnErr = nil
			}
		})
		if returnErr != nil {
			return false, returnErr
		}
		if held == string(value) {
			return false, nil
		}
	}
	return set, tlsSuspensionWriteKey(ctx, networkId, host, value)
}

// Lifts the downgrade's kill of host: the key's field while it still holds this job's entry, with the trigger,
// then the row while its reason is still this job's. A kill an abuse case set over it since keeps both. The key
// goes first, so a pass killed between the two leaves the row, which keeps the kill and which the next pass
// lifts. Answers whether the row went.
func liftTlsDowngradeSuspension(ctx context.Context, networkId server.Id, host string) (lifted bool, returnErr error) {
	value, err := tlsSuspensionEntryValue(host, TlsSuspensionReasonQuotaDowngrade, time.Time{})
	if err != nil {
		return false, err
	}
	keys := []string{
		sidecar.TlsKillKey(sidecar.Id(networkId)),
		sidecar.TlsKillTriggerKey(sidecar.Id(networkId)),
	}
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = tlsSuspensionExpireScript.Run(ctx, r, keys, host, string(value)).Err()
	})
	if returnErr != nil {
		return false, returnErr
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_tls_suspension
				WHERE network_id = $1 AND host = $2 AND reason = $3
			`,
			networkId,
			host,
			TlsSuspensionReasonQuotaDowngrade,
		))
		lifted = tag.RowsAffected() == 1
	})
	return lifted, nil
}
