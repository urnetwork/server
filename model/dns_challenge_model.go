package model

// DNS-01's challenges (DNS-M5-13; R-DNS-7, D-DNS-8): what POST
// /dns/internal/acme-challenge writes for certs, the only DNS-01 writer, which
// has already checked the caller's §8 grant.
//
//   - The TXT at _acme-challenge.<name>.<root>, <name> a live label or a live
//     alias (§12.7; reading 11: under a claimed alias before its other records
//     answer), holds at most two values: a wildcard's and the apex's, which
//     validate at one name (RFC 8555 §8.4). TTL §12.6's challenge, 60.
//   - At most dns_challenges_per_name_hour writes a name an hour (§12.7's 10,
//     pro.yml's sidecar_abuse:), counted from the journal's last hour, so Let's
//     Encrypt's failed-authorization limit is never reached by our retries. A
//     removal is never counted or refused.
//   - Every write takes a new serial, the same values again included: certs
//     writes again after /dns/ready answers stale, and that rewrite must reach a
//     serial the restored edges serve (§13.8).
//   - A row expires an hour after its last write (dnsCommitInTx), and taskworker
//     removes it through the journal (RemoveExpiredDnsChallenges).
//   - Challenges are free of dns_entries and add no admin event (§12.7, §12.8).
//
// Nothing here runs unless the binary links MG2 and MG5.

import (
	"context"
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
	// The name's writes this hour reached dns_challenges_per_name_hour.
	ErrDnsChallengeRate = errors.New("dns challenge rate reached")
)

// The label every challenge sits under (RFC 8555 §8.4).
const DnsChallengeLabel = "_acme-challenge"

// The most values one challenge RRset holds: a wildcard's and the apex's.
const dnsChallengeValueMaxCount = 2

// The longest value a TXT string holds.
const dnsChallengeValueMaxByteCount = 255

// The window dns_challenges_per_name_hour counts over.
const dnsChallengeRateWindow = 1 * time.Hour

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
// alias it is.
func resolveDnsChallengeNameInTx(ctx context.Context, query server.PgCanQuery, root string, name string) (string, server.Id, error) {
	root = dnsLabelRoot(root)
	name = strings.TrimPrefix(dns.CanonicalName(name), "*.")
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

// The values as TXT rdata: each printable ASCII with no quote or backslash, at
// most 255 bytes, no value twice.
func dnsChallengeRdata(values []string) ([]string, error) {
	if dnsChallengeValueMaxCount < len(values) {
		return nil, fmt.Errorf("%w: %d values", ErrDnsChallengeValues, len(values))
	}
	rdata := []string{}
	for _, value := range values {
		if value == "" || dnsChallengeValueMaxByteCount < len(value) {
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
	if limit <= int64(countDnsChallengeWritesInTx(ctx, tx, root, networkId, owner, server.NowUtc().Add(-dnsChallengeRateWindow))) {
		return nil, fmt.Errorf("%w: %s", ErrDnsChallengeRate, owner)
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

// How many journal rows since since wrote owner's challenge: the network's
// rows of the window, through its index, matched by containment.
func countDnsChallengeWritesInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id, owner string, since time.Time) int {
	match := fmt.Sprintf(`{"upserts":[{"owner":%q,"type":%d}]}`, owner, dns.TypeTXT)
	count := 0
	result, err := query.Query(
		ctx,
		`
			SELECT count(*) FROM dns_zone_journal
			WHERE network_id = $1 AND $2 <= commit_time AND root = $3 AND diff @> $4::jsonb
		`,
		networkId,
		since.UTC(),
		root,
		match,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count))
		}
	})
	return count
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
