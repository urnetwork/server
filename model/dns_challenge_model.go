package model

// DNS-01's challenges (DNS-M5-13; R-DNS-7, D-DNS-8): what POST
// /dns/internal/acme-challenge writes for certs, the only DNS-01 writer, which
// has already checked the caller's §8 grant.
//
//   - The TXT at _acme-challenge.<name>.<root>, <name> a live label or a live
//     alias (§12.7; reading 11: under a claimed alias before its other records
//     answer), holds at most two values: a wildcard's and the apex's, which
//     validate at one name (RFC 8555 §8.4). TTL §12.6's challenge, 60.
//   - A verified custom domain's challenges sit at <acme_token>.acme.<root>
//     itself, the target of the domain's one-time _acme-challenge CNAME
//     (QD-175; DNS-M6-02): one of <root>'s own rows, under the same rules, for
//     a token a verified network_domain holds.
//   - At most dns_challenges_per_name_hour writes a name an hour (§12.7's 10,
//     pro.yml's sidecar_abuse:), counted from the journal's last hour, so Let's
//     Encrypt's failed-authorization limit is never reached by our retries. A
//     removal is never counted or refused. A refusal says how long until the
//     name takes a write again (DnsChallengeRateError), which the route's 429
//     names in its Retry-After and certs holds the name's orders for (QD-1204).
//   - Every write takes a new serial, the same values again included: certs
//     writes again after /dns/ready answers stale, and that rewrite must reach a
//     serial the restored edges serve (§13.8).
//   - A row expires an hour after its last write (dnsCommitInTx), and taskworker
//     removes it through the journal (RemoveExpiredDnsChallenges).
//   - Challenges are free of dns_entries and add no admin event (§12.7, §12.8).
//   - A write takes no idempotency key: every write is a new serial. So what certs needs to settle a write whose
//     answer it never heard (QD-1165) is here: the journal's commit of a value at a custom domain's challenge
//     target above a serial, read under the zone's lock (FindDnsAcmeChallengeCommitInTx), and the target's writes
//     in the cap's window (GetDnsAcmeChallengeWindowInTx).
//
// Nothing here runs unless the binary links MG2 and MG5.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

var (
	// The name is no live label or alias of the root.
	ErrDnsChallengeName = errors.New("dns challenge name names no live label")
	// More than two values, or a value that is no TXT string.
	ErrDnsChallengeValues = errors.New("dns challenge values refused")
	// The name's writes this hour reached dns_challenges_per_name_hour. The write path's refusal is a
	// *DnsChallengeRateError, which is this.
	ErrDnsChallengeRate = errors.New("dns challenge rate reached")
)

// The cap's refusal of a write at Owner, with how long until the name takes a write again (QD-1204).
type DnsChallengeRateError struct {
	// The challenge's owner name, _acme-challenge.<name> or a custom domain's target.
	Owner string
	// Until the write whose leaving brings the name's count in the window below the cap has left it, the
	// oldest when the count is at the cap, rounded up to the whole second past that, since the window counts
	// a write at its edge: at least a second. Zero when no write's leaving lets one in, as with a cap of
	// zero.
	RetryAfter time.Duration
}

func (self *DnsChallengeRateError) Error() string {
	return fmt.Sprintf("%s: %s", ErrDnsChallengeRate, self.Owner)
}

func (self *DnsChallengeRateError) Unwrap() error {
	return ErrDnsChallengeRate
}

// The label every challenge sits under (RFC 8555 §8.4).
const DnsChallengeLabel = "_acme-challenge"

// What a challenge may hold, and the window its cap counts over (§12.8, R-DNS-7).
type DnsChallengeSettings struct {
	// The most values one challenge RRset holds: a wildcard's and the apex's, which validate at one name (RFC 8555
	// §8.4). A protocol limit: certs never writes more.
	ValueMaxCount int
	// The longest value a TXT string holds (RFC 1035 §3.3.14).
	ValueMaxByteCount int
	// The window dns_challenges_per_name_hour counts a name's writes over.
	RateWindow time.Duration
}

func DefaultDnsChallengeSettings() *DnsChallengeSettings {
	return &DnsChallengeSettings{
		ValueMaxCount:     2,
		ValueMaxByteCount: 255,
		RateWindow:        1 * time.Hour,
	}
}

// What one challenge write names.
type DnsChallengeArgs struct {
	Root string
	// The name the challenges validate: <label>.<root> or <alias>.<root>, with
	// or without a *. and a final dot.
	Name string
	// At most two key authorizations' digests. None removes the challenge.
	Values []string
	// Seconds: dns.yml's ttl.challenge. Zero takes the process's dns.yml, or
	// §12.6's 60 without one.
	Ttl uint32
}

// The challenges' cap a name an hour, through pro.yml's sidecar_abuse:. A
// missing value reads zero, which refuses every write: the closed direction.
var dnsChallengesPerNameHour = func() int64 {
	return Pro().SidecarAbuse.Limit("dns_challenges_per_name_hour")
}

// The challenges' cap a name an hour, as the write path reads it.
func DnsChallengesPerNameHour() int64 {
	return dnsChallengesPerNameHour()
}

// Sets the cap's source for a test, returning what puts it back.
func Testing_SetDnsChallengesPerNameHour(limit int64) (restore func()) {
	previous := dnsChallengesPerNameHour
	dnsChallengesPerNameHour = func() int64 {
		return limit
	}
	return func() {
		dnsChallengesPerNameHour = previous
	}
}

// The challenge owner of name in root, and the network whose live label or
// alias it is: zero for a custom domain's <acme_token>.acme.<root>, which is
// <root>'s own row.
func resolveDnsChallengeNameInTx(ctx context.Context, query server.PgCanQuery, root string, name string) (string, server.Id, error) {
	root = dnsLabelRoot(root)
	name = strings.TrimPrefix(dns.CanonicalName(name), "*.")
	if acmeToken, ok := strings.CutSuffix(name, "."+dnsAcmeChallengeLabel+"."+root); ok {
		return resolveDnsAcmeChallengeTargetInTx(ctx, query, name, acmeToken)
	}
	label, rest, found := strings.Cut(name, ".")
	if !found || rest != root || label == "" {
		return "", server.Id{}, fmt.Errorf("%w: %s", ErrDnsChallengeName, name)
	}
	var networkId *server.Id
	result, err := query.Query(
		ctx,
		`SELECT network_id FROM dns_label WHERE root = $1 AND label = $2 AND state = $3`,
		root,
		label,
		DnsLabelStateLive,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&networkId))
		}
	})
	if networkId == nil {
		return "", server.Id{}, fmt.Errorf("%w: %s", ErrDnsChallengeName, name)
	}
	return DnsChallengeLabel + "." + name, *networkId, nil
}

// A custom domain's challenge target, <acme_token>.acme.<root>, for a token a
// verified network_domain holds: the name itself, in <root>'s own rows (QD-175).
func resolveDnsAcmeChallengeTargetInTx(ctx context.Context, query server.PgCanQuery, name string, acmeToken string) (string, server.Id, error) {
	if !server.SidecarMg6Linked() || acmeToken == "" || strings.Contains(acmeToken, ".") {
		return "", server.Id{}, fmt.Errorf("%w: %s", ErrDnsChallengeName, name)
	}
	held := false
	result, err := query.Query(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM network_domain WHERE acme_token = $1 AND verify_time IS NOT NULL)`,
		acmeToken,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&held))
		}
	})
	if !held {
		return "", server.Id{}, fmt.Errorf("%w: %s", ErrDnsChallengeName, name)
	}
	return name, server.Id{}, nil
}

// The values as TXT rdata: each printable ASCII with no quote or backslash, at
// most ValueMaxByteCount bytes, no value twice.
func dnsChallengeRdata(values []string) ([]string, error) {
	settings := DefaultDnsChallengeSettings()
	if settings.ValueMaxCount < len(values) {
		return nil, fmt.Errorf("%w: %d values", ErrDnsChallengeValues, len(values))
	}
	rdata := []string{}
	for _, value := range values {
		if value == "" || settings.ValueMaxByteCount < len(value) {
			return nil, fmt.Errorf("%w: a value of %d bytes", ErrDnsChallengeValues, len(value))
		}
		for index := range len(value) {
			if value[index] < 0x21 || 0x7e < value[index] || value[index] == '"' || value[index] == '\\' {
				return nil, fmt.Errorf("%w: a value that is no token", ErrDnsChallengeValues)
			}
		}
		quoted := `"` + value + `"`
		if !slices.Contains(rdata, quoted) {
			rdata = append(rdata, quoted)
		}
	}
	return rdata, nil
}

// Writes or removes name's challenge in the caller's transaction (see the file
// header) and returns the write. A removal of a challenge already gone answers
// dnsservice.ErrDnsWriteEmpty, which the route treats as done.
func DnsPutChallengeInTx(ctx context.Context, tx server.PgTx, args *DnsChallengeArgs) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	root := dnsLabelRoot(args.Root)
	rdata, err := dnsChallengeRdata(args.Values)
	if err != nil {
		return nil, err
	}
	owner, networkId, err := resolveDnsChallengeNameInTx(ctx, tx, root, args.Name)
	if err != nil {
		return nil, err
	}
	key := dnsservice.DnsRecordKey{Owner: owner, Type: dns.TypeTXT}
	if len(rdata) == 0 {
		return DnsWriteInTx(ctx, tx, &DnsWriteArgs{Root: root, NetworkId: networkId, Deletes: []dnsservice.DnsRecordKey{key}})
	}
	ttl := args.Ttl
	if ttl == 0 {
		ttl = dnsZoneTtl().Challenge
	}
	// the zone's lock first, so two writes for one name count each other
	lockDnsZoneInTx(ctx, tx, root, networkId)
	limit := dnsChallengesPerNameHour()
	now := server.NowUtc()
	count, freeTime := countDnsChallengeWritesInTx(ctx, tx, root, networkId, owner, now, DefaultDnsChallengeSettings().RateWindow, limit)
	if limit <= int64(count) {
		rateErr := &DnsChallengeRateError{Owner: owner}
		if !freeTime.IsZero() {
			// the window counts a write at its edge, so the name takes one only past freeTime
			rateErr.RetryAfter = (freeTime.Sub(now)/time.Second + 1) * time.Second
		}
		return nil, rateErr
	}
	return DnsWriteInTx(ctx, tx, &DnsWriteArgs{
		Root:      root,
		NetworkId: networkId,
		Upserts: []dnsservice.DnsRecordSet{{
			Owner: owner,
			Type:  dns.TypeTXT,
			Ttl:   ttl,
			Kind:  dnsservice.DnsRecordKindChallenge,
			Rdata: rdata,
		}},
	})
}

// DnsPutChallengeInTx in a transaction of its own, whose commit raises the
// serial.
func DnsPutChallenge(ctx context.Context, args *DnsChallengeArgs) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsPutChallengeInTx(ctx, tx, args)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, err
	}
	return result, returnErr
}

// How many journal rows in the window ending at now wrote owner's challenge, and when the name takes a
// write again under limit: window past the commit of the row whose leaving brings the count below limit,
// the oldest row's when the count is at limit (QD-1204). Zero while the count is below limit, and when no
// row's leaving brings it there, as with a limit of zero. The network's rows of the window, through its
// index, matched by containment: at most about limit of them match.
func countDnsChallengeWritesInTx(
	ctx context.Context,
	query server.PgCanQuery,
	root string,
	networkId server.Id,
	owner string,
	now time.Time,
	window time.Duration,
	limit int64,
) (int, time.Time) {
	match := fmt.Sprintf(`{"upserts":[{"owner":%q,"type":%d}]}`, owner, dns.TypeTXT)
	since := now.Add(-window).UTC()
	// a network's rows through its index; <root>'s own, a custom domain's challenge target among them, write
	// with no network, through the commit time's
	sql := `
		SELECT commit_time FROM dns_zone_journal
		WHERE network_id = $1 AND $2 <= commit_time AND root = $3 AND diff @> $4::jsonb
		ORDER BY commit_time
	`
	args := []any{networkId, since, root, match}
	if networkId == (server.Id{}) {
		sql = `
			SELECT commit_time FROM dns_zone_journal
			WHERE network_id IS NULL AND $1 <= commit_time AND root = $2 AND diff @> $3::jsonb
			ORDER BY commit_time
		`
		args = []any{since, root, match}
	}
	commitTimes := []time.Time{}
	result, err := query.Query(ctx, sql, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var commitTime time.Time
			server.Raise(result.Scan(&commitTime))
			commitTimes = append(commitTimes, commitTime.UTC())
		}
	})
	count := len(commitTimes)
	if limit <= 0 || int64(count) < limit {
		return count, time.Time{}
	}
	// below limit once count - limit + 1 rows have left, this one the last of them
	return count, commitTimes[int64(count)-limit].Add(window)
}

// A custom domain's challenge target in root as the journal names its owner, name canonical with no *., for a
// name <acme_token>.acme.<root>; false for any other name.
func dnsAcmeChallengeOwner(root string, name string) (string, bool) {
	root = dnsLabelRoot(root)
	owner := strings.TrimPrefix(dns.CanonicalName(name), "*.")
	acmeToken, ok := strings.CutSuffix(owner, "."+dnsAcmeChallengeLabel+"."+root)
	if !ok || acmeToken == "" || strings.Contains(acmeToken, ".") {
		return "", false
	}
	return owner, true
}

// The serial of the journal's first commit above afterSerial that upserts value in the TXT at name, a custom
// domain's challenge target in root (QD-175), or zero for none: how certs learns that a write whose answer it never
// heard was made (QD-1165). The zone's lock comes first, the lock every write of <root>'s own rows holds through its
// commit, so a write inside its transaction has ended and its row is read; tx must be read committed, so the read
// sees what committed before the lock. Any other name answers zero.
func FindDnsAcmeChallengeCommitInTx(ctx context.Context, tx server.PgTx, root string, name string, value string, afterSerial uint64) uint64 {
	owner, ok := dnsAcmeChallengeOwner(root, name)
	if !ok {
		return 0
	}
	root = dnsLabelRoot(root)
	lockDnsZoneInTx(ctx, tx, root, server.Id{})
	match, err := json.Marshal(map[string]any{
		"upserts": []any{map[string]any{"owner": owner, "type": dns.TypeTXT, "rdata": []string{`"` + value + `"`}}},
	})
	server.Raise(err)
	var serial uint64
	result, err := tx.Query(
		ctx,
		`
			SELECT serial FROM dns_zone_journal
			WHERE root = $1 AND network_id IS NULL AND $2 < serial AND diff @> $3::jsonb
			ORDER BY serial
			LIMIT 1
		`,
		root,
		dnsSerialColumn(afterSerial),
		string(match),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var serialColumn int64
			server.Raise(result.Scan(&serialColumn))
			serial = dnsSerialFromColumn(serialColumn)
		}
	})
	return serial
}

// How many writes name, a custom domain's challenge target in root, took in the cap's window ending at now, and
// when the oldest of them leaves the window, zero with none: what the write path counts against
// dns_challenges_per_name_hour, which certs reads before it charges a write (QD-1165). Any other name took none.
func GetDnsAcmeChallengeWindowInTx(ctx context.Context, query server.PgCanQuery, root string, name string, now time.Time) (int, time.Time) {
	owner, ok := dnsAcmeChallengeOwner(root, name)
	if !ok {
		return 0, time.Time{}
	}
	root = dnsLabelRoot(root)
	window := DefaultDnsChallengeSettings().RateWindow
	match, err := json.Marshal(map[string]any{"upserts": []any{map[string]any{"owner": owner, "type": dns.TypeTXT}}})
	server.Raise(err)
	count := 0
	var oldest *time.Time
	result, err := query.Query(
		ctx,
		`
			SELECT count(*), min(commit_time) FROM dns_zone_journal
			WHERE network_id IS NULL AND $1 <= commit_time AND root = $2 AND diff @> $3::jsonb
		`,
		now.Add(-window).UTC(),
		root,
		string(match),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count, &oldest))
		}
	})
	if oldest == nil {
		return count, time.Time{}
	}
	return count, oldest.UTC().Add(window)
}

// Removes every challenge and canary past its hour at now, through the journal,
// one write a zone (D-DNS-8). Returns how many went and whether more wait. A
// zone that cannot be written now, restoring or suspended, keeps its rows for
// the next pass.
func RemoveExpiredDnsChallenges(ctx context.Context, now time.Time) (int, bool, error) {
	if !server.SidecarMg5Linked() {
		return 0, false, nil
	}
	type zoneKey struct {
		root      string
		networkId server.Id
	}
	zoneKeys := map[zoneKey][]dnsservice.DnsRecordKey{}
	rowCount := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT root, network_id, owner, rrtype
				FROM network_dns_record
				WHERE expire_time < $1
				ORDER BY expire_time
				LIMIT $2
			`,
			now.UTC(),
			DefaultDnsModelSettings().ExpiryBatchSize,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var root string
				var networkId *server.Id
				var owner string
				var rrType int32
				server.Raise(result.Scan(&root, &networkId, &owner, &rrType))
				key := zoneKey{root: root}
				if networkId != nil {
					key.networkId = *networkId
				}
				zoneKeys[key] = append(zoneKeys[key], dnsservice.DnsRecordKey{Owner: owner, Type: uint16(rrType)})
				rowCount += 1
			}
		})
	})
	removedCount := 0
	errs := []error{}
	for key, deletes := range zoneKeys {
		_, err := DnsWrite(ctx, &DnsWriteArgs{Root: key.root, NetworkId: key.networkId, Deletes: deletes})
		switch {
		case err == nil:
			removedCount += len(deletes)
		case errors.Is(err, dnsservice.ErrDnsWriteEmpty):
			// gone already
		case errors.Is(err, ErrDnsZoneSuspended):
			// a suspended zone's rows are withheld, so no edge holds them, and
			// they go with no journal row
			server.Tx(ctx, func(tx server.PgTx) {
				tag := server.RaisePgResult(tx.Exec(
					ctx,
					`
						DELETE FROM network_dns_record
						WHERE root = $1 AND network_id = $2 AND expire_time < $3 AND withheld_time IS NOT NULL
					`,
					key.root,
					key.networkId,
					now.UTC(),
				))
				removedCount += int(tag.RowsAffected())
			})
		default:
			errs = append(errs, err)
		}
	}
	return removedCount, rowCount == DefaultDnsModelSettings().ExpiryBatchSize, errors.Join(errs...)
}
