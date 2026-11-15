package model

// §12.5's aliases (DNS-M6-01; R-DNS-5, D-DNS-2, D-DNS-10, X5, I-14): a network's opt-in second name,
// <alias>.<root>, equal to its network_name, "public forever (CT)".
//
//   - ClaimDnsAlias, POST /network/dns/alias-claim: the caller names its network_name as it stands, which
//     must pass ValidateDnsName and ScreenName under the network's facts, with network_verified and no
//     probation, and no dns_label row may hold it in any state, since labels and aliases share the primary
//     key (root, label). The claim writes the alias row with no rdata, which holds the alias's apex for its
//     own challenges alone (dnsservice zone_alias.go), and moves the label map's cursor, so tlsedge routes
//     the alias and certs can prove it. A network holding another live alias changes it: the old one retires
//     in the same transaction.
//   - DnsAliasCertifiedInTx, which TLS-M6-03 calls when a certificate naming the alias issues (reading
//     11): dns_label.certified_time is set and the alias row takes its label's apex, so every name below the
//     alias answers the label's records from then on, through the journal.
//   - ReleaseDnsAlias, POST /network/dns/alias-release: the alias retires. It keeps answering for D-DNS-10's
//     90 days, outlasting senders' 5-day retries, under <root>'s CAA issue ";" at its apex, which refuses
//     every new certificate for it, and every issued certificate naming it is marked for revocation (§13.8,
//     TLS-M5-16's RequestTlsCertRevocation).
//   - TombstoneRetiredDnsAliases, in taskworker's dns_maintenance: an alias retiring 90 days tombstones for
//     good, and its rows and its CAA leave the root in one journal write. The CAA of an alias a network's
//     deletion tombstoned at once (DnsWithdrawNetworkInTx) leaves too.
//
// Renaming network_name changes nothing here. Every writer takes the network's label lock, then its zone's
// lock, then <root>'s own zone's lock, and nothing here runs unless the binary links MG2, MG5 and MG6, whose
// dns_label.certified_time an alias reads.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The admin events of an alias (§7.11): the network is the target, and no name is said (I-13).
const (
	AdminEventActionDnsAliasClaim     = "dns.alias_claim"
	AdminEventActionDnsAliasRelease   = "dns.alias_release"
	AdminEventActionDnsAliasTombstone = "dns.alias_tombstone"
)

// A retiring alias's CAA at its apex (D-DNS-10): no CA may issue for it.
const dnsAliasRetiringCaa = `0 issue ";"`

// The alias is no live alias of the network.
var ErrDnsAliasNotLive = errors.New("dns alias not live")

// Tunables of aliases.
type DnsAliasSettings struct {
	// How long a released or changed alias retires before it tombstones: D-DNS-10's 90 days, outlasting
	// senders' 5-day retries and every certificate that named it.
	RetiringWindow time.Duration
	// The most aliases one tombstone pass takes, and the most orphaned CAA rows one pass removes, so a
	// backlog costs bounded passes.
	SweepBatchCount int
}

func DefaultDnsAliasSettings() *DnsAliasSettings {
	return &DnsAliasSettings{
		RetiringWindow:  90 * 24 * time.Hour,
		SweepBatchCount: 100,
	}
}

// One claim or release as a route takes it.
type DnsAliasRequest struct {
	Root      string
	NetworkId server.Id
	// The alias named: the network's network_name for a claim, its live alias for a release.
	Name string
	// The admin event's actor (§7.11).
	ActorKind string
	ActorId   *server.Id
	RequestId *server.Id
}

// An admin event's actor kind as a request names it, the system's when it names none, as a taskworker's
// call does.
func dnsAdminEventActorKind(actorKind string) string {
	if actorKind == "" {
		return AdminEventActorSystem
	}
	return actorKind
}

// The binary links what an alias reads.
func dnsAliasLinked() bool {
	return server.SidecarMg2Linked() && server.SidecarMg5Linked() && server.SidecarMg6Linked()
}

// One alias row of dns_label.
type dnsAliasRow struct {
	name          string
	state         string
	stateTime     time.Time
	certifiedTime *time.Time
}

// The network's alias rows in root, by state, live and retiring, newest first.
func getNetworkDnsAliasRowsInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) []*dnsAliasRow {
	rows := []*dnsAliasRow{}
	result, err := query.Query(
		ctx,
		`
			SELECT label, state, state_time, certified_time FROM dns_label
			WHERE network_id = $1 AND root = $2 AND kind = $3 AND state IN ($4, $5)
			ORDER BY state_time DESC, label
		`,
		networkId,
		root,
		DnsLabelKindAlias,
		DnsLabelStateLive,
		DnsLabelStateRetiring,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			row := &dnsAliasRow{}
			server.Raise(result.Scan(&row.name, &row.state, &row.stateTime, &row.certifiedTime))
			rows = append(rows, row)
		}
	})
	return rows
}

// The network's live alias in root, nil for none.
func getNetworkLiveDnsAliasInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) *dnsAliasRow {
	for _, row := range getNetworkDnsAliasRowsInTx(ctx, query, root, networkId) {
		if row.state == DnsLabelStateLive {
			return row
		}
	}
	return nil
}

// The answer of the alias routes for row.
func dnsAliasAnswer(root string, row *dnsAliasRow, settings *DnsAliasSettings) *sidecar.DnsAliasAnswer {
	answer := &sidecar.DnsAliasAnswer{
		Name:       row.name + "." + strings.TrimSuffix(root, "."),
		State:      sidecar.DnsAliasStateLive,
		Certified:  row.certifiedTime != nil,
		Disclosure: sidecar.DnsAliasDisclosure,
	}
	if row.state == DnsLabelStateRetiring {
		answer.State = sidecar.DnsAliasStateRetiring
		tombstoneTime := row.stateTime.Add(settings.RetiringWindow).UTC()
		answer.TombstoneTime = &tombstoneTime
	}
	return answer
}

// The network's name as it stands, "" for a network that is gone.
func getNetworkNameInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id) string {
	name := ""
	result, err := query.Query(ctx, `SELECT network_name FROM network WHERE network_id = $1`, networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&name))
		}
	})
	return name
}

// Takes the network's label lock to the end of the transaction, as EnsureNetworkLabelInTx takes it.
func lockDnsLabelInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('dns_label:' || $1::uuid::text, 0))`,
		networkId,
	))
}

// The alias row at the alias's apex: holding its label's apex once certified, nothing before (reading
// 11).
func dnsAliasRecordSet(root string, alias string, labelApex string) dnsservice.DnsRecordSet {
	set := dnsservice.DnsRecordSet{Owner: alias + "." + root, Type: dnsservice.DnsAliasType, Kind: dnsservice.DnsRecordKindAlias}
	if labelApex != "" {
		set.Rdata = []string{labelApex}
	}
	return set
}

// §12.5's claim (see the file header): the alias's answer, or a *sidecar.DnsApiError with nothing written.
func ClaimDnsAlias(ctx context.Context, request *DnsAliasRequest) (*sidecar.DnsAliasAnswer, error) {
	if !dnsAliasLinked() {
		return nil, dnsRecordsUnavailable("the alias tables are not linked")
	}
	if request == nil || request.NetworkId == (server.Id{}) {
		return nil, dnsRecordsInvalid("no caller")
	}
	root := dnsLabelRoot(request.Root)
	name := request.Name
	if err := sidecar.ValidateDnsName(name); err != nil {
		return nil, dnsRecordsInvalid("the alias %v", err)
	}
	var answer *sidecar.DnsAliasAnswer
	var change *dnsAliasChange
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			answer, change, returnErr = claimDnsAliasInTx(ctx, tx, root, name, request)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, dnsRecordsApiError(err)
	}
	// once a change has committed: every issued certificate naming the old alias revoked (§13.8)
	if change != nil {
		if err := revokeDnsAliasCertificates(ctx, root, request.NetworkId, change.label, change.retired); err != nil {
			return nil, err
		}
	}
	return answer, nil
}

// A claim that changed the network's alias: the old one retired.
type dnsAliasChange struct {
	label   string
	retired string
}

func claimDnsAliasInTx(ctx context.Context, tx server.PgTx, root string, name string, request *DnsAliasRequest) (*sidecar.DnsAliasAnswer, *dnsAliasChange, error) {
	networkId := request.NetworkId
	settings := DefaultDnsAliasSettings()
	abuse, _ := currentDnsRecordsDeps()
	label, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	if err != nil {
		return nil, nil, err
	}
	if networkName := getNetworkNameInTx(ctx, tx, networkId); networkName != name {
		return nil, nil, dnsRecordsError(http.StatusConflict, sidecar.DnsApiCodeRefused, "an alias is the network's name, which is not %q", name)
	}
	standing, ok := abuse.Standing(connect.Id(networkId))
	switch {
	case !ok || !standing.Known():
		return nil, nil, dnsRecordsUnavailable("the network's standing is not loaded")
	case standing == sidecar.AbuseStandingSuspended:
		return nil, nil, dnsRecordsRefused("the network is suspended (§21.7)")
	case standing == sidecar.AbuseStandingProbation || standing == sidecar.AbuseStandingRestricted:
		return nil, nil, dnsRecordsRefused("a network on probation claims no alias (§21.7)")
	}
	verified, ok := abuse.Verified(connect.Id(networkId))
	if !ok {
		return nil, nil, dnsRecordsUnavailable("the network's verification is not loaded")
	}
	if !verified {
		return nil, nil, sidecar.NewDnsQuotaError(&sidecar.QuotaRefusal{LimitKey: DnsVerificationLimitKey, Upgrade: true})
	}
	facts := &sidecar.ScreenFacts{VerifiedDomains: getNetworkVerifiedDomainsInTx(ctx, tx, networkId)}
	if verdict := sidecar.ScreenName(name, facts); verdict != sidecar.ScreenVerdictAllow {
		return nil, nil, dnsRecordsRefused("the name %s is refused (§21.6)", name)
	}
	lockDnsZoneInTx(ctx, tx, root, networkId)
	now := server.NowUtc()
	current := getNetworkLiveDnsAliasInTx(ctx, tx, root, networkId)
	if current != nil && current.name == name {
		return dnsAliasAnswer(root, current, settings), nil, nil
	}
	// the zone on first need, so the alias has its label's records to answer once a certificate names it
	if _, err := EnsureNetworkZoneInTx(ctx, tx, networkId, root, nil); err != nil {
		return nil, nil, err
	}
	var change *dnsAliasChange
	if current != nil {
		// a change: the old alias retires
		if _, err := retireDnsAliasInTx(ctx, tx, root, networkId, current.name, now); err != nil {
			return nil, nil, err
		}
		change = &dnsAliasChange{label: label, retired: current.name}
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO dns_label (root, label, network_id, kind, state, state_time)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (root, label) DO NOTHING
		`,
		root,
		name,
		networkId,
		DnsLabelKindAlias,
		DnsLabelStateLive,
		now,
	))
	if tag.RowsAffected() != 1 {
		return nil, nil, dnsRecordsError(http.StatusConflict, sidecar.DnsApiCodeRefused, "the name %s is held, and never reassigned (§12.5)", name)
	}
	result, err := DnsWriteInTx(ctx, tx, &DnsWriteArgs{
		Root:      root,
		NetworkId: networkId,
		Upserts:   []dnsservice.DnsRecordSet{dnsAliasRecordSet(root, name, "")},
	})
	if err != nil {
		return nil, nil, err
	}
	touchDnsLabelMapInTx(ctx, tx, root, networkId, result.Serial)
	targetId := networkId
	AddAdminEventInTx(ctx, tx, &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  dnsAdminEventActorKind(request.ActorKind),
		ActorId:    request.ActorId,
		Action:     AdminEventActionDnsAliasClaim,
		TargetKind: AdminEventTargetDnsLabel,
		TargetId:   &targetId,
		Result:     AdminEventResultOk,
		RequestId:  request.RequestId,
	})
	return dnsAliasAnswer(root, &dnsAliasRow{name: name, state: DnsLabelStateLive, stateTime: now}, settings), change, nil
}

// The alias retires in the caller's transaction, which holds the network's label and zone locks: its row
// goes retiring, starting D-DNS-10's 90 days, and <root>'s CAA issue ";" stands at its apex. The alias row
// stays, so a certified alias keeps answering for senders' retries. Returns the root write.
func retireDnsAliasInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id, alias string, now time.Time) (*DnsWriteResult, error) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE dns_label SET state = $5, state_time = $6
			WHERE root = $1 AND label = $2 AND network_id = $3 AND kind = $4 AND state = $7
		`,
		root,
		alias,
		networkId,
		DnsLabelKindAlias,
		DnsLabelStateRetiring,
		now,
		DnsLabelStateLive,
	))
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("%w: %s", ErrDnsAliasNotLive, alias)
	}
	return DnsWriteInTx(ctx, tx, &DnsWriteArgs{
		Root: root,
		Upserts: []dnsservice.DnsRecordSet{{
			Owner: alias + "." + root,
			Type:  dns.TypeCAA,
			Ttl:   dnsZoneTtl().Caa,
			Kind:  dnsservice.DnsRecordKindSystem,
			Rdata: []string{dnsAliasRetiringCaa},
		}},
	})
}

// §12.5's release (see the file header): the alias's answer, retiring, or a *sidecar.DnsApiError with
// nothing written. A release of an alias already retiring answers it again, and asks again for the
// revocation of every issued certificate naming it.
func ReleaseDnsAlias(ctx context.Context, request *DnsAliasRequest) (*sidecar.DnsAliasAnswer, error) {
	if !dnsAliasLinked() {
		return nil, dnsRecordsUnavailable("the alias tables are not linked")
	}
	if request == nil || request.NetworkId == (server.Id{}) {
		return nil, dnsRecordsInvalid("no caller")
	}
	root := dnsLabelRoot(request.Root)
	name := dnsLabelText(root, request.Name)
	settings := DefaultDnsAliasSettings()
	var answer *sidecar.DnsAliasAnswer
	label := ""
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			answer, returnErr = nil, nil
			lockDnsLabelInTx(ctx, tx, request.NetworkId)
			label = getNetworkLabelInTx(ctx, tx, request.NetworkId, root)
			var row *dnsAliasRow
			for _, aliasRow := range getNetworkDnsAliasRowsInTx(ctx, tx, root, request.NetworkId) {
				if aliasRow.name == name {
					row = aliasRow
					break
				}
			}
			switch {
			case row == nil:
				returnErr = dnsRecordsError(http.StatusNotFound, sidecar.DnsApiCodeInvalid, "%s is no alias of the network", name)
			case row.state == DnsLabelStateRetiring:
				answer = dnsAliasAnswer(root, row, settings)
			default:
				lockDnsZoneInTx(ctx, tx, root, request.NetworkId)
				now := server.NowUtc()
				result, err := retireDnsAliasInTx(ctx, tx, root, request.NetworkId, name, now)
				if err != nil {
					returnErr = err
					break
				}
				touchDnsLabelMapInTx(ctx, tx, root, request.NetworkId, result.Serial)
				targetId := request.NetworkId
				AddAdminEventInTx(ctx, tx, &AdminEvent{
					NetworkId:  request.NetworkId,
					ActorKind:  dnsAdminEventActorKind(request.ActorKind),
					ActorId:    request.ActorId,
					Action:     AdminEventActionDnsAliasRelease,
					TargetKind: AdminEventTargetDnsLabel,
					TargetId:   &targetId,
					Result:     AdminEventResultOk,
					RequestId:  request.RequestId,
				})
				row.state = DnsLabelStateRetiring
				row.stateTime = now
				answer = dnsAliasAnswer(root, row, settings)
			}
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, dnsRecordsApiError(err)
	}
	// once the release has committed: every issued certificate naming the alias revoked (§13.8)
	if err := revokeDnsAliasCertificates(ctx, root, request.NetworkId, label, name); err != nil {
		return nil, err
	}
	return answer, nil
}

// Marks every issued certificate of the network whose identifier set named alias beside label for
// revocation, cessationOfOperation, through TLS-M5-16's RequestTlsCertRevocation, which TLS-M5-17's chain
// takes to the CA. A network-level device's set names each live alias's pair (§13.8), and a network holds
// one alias at a time, so such a certificate's set is exactly {label, alias}'s: a certificate ordered since,
// without the alias, has another set and is left. Each mark is its own transaction, after the release's.
func revokeDnsAliasCertificates(ctx context.Context, root string, networkId server.Id, label string, alias string) error {
	if label == "" {
		return nil
	}
	identifierSet, err := sidecar.TlsIdentifierSet(strings.TrimSuffix(root, "."), label, "", []string{alias})
	if err != nil {
		return err
	}
	identifierSetSha256, err := sidecar.TlsIdentifierSetHash(identifierSet)
	if err != nil {
		return err
	}
	certIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT cert_id FROM network_tls_cert
				WHERE network_id = $1 AND state = 'issued' AND identifier_set_sha256 = $2
				ORDER BY cert_id
			`,
			networkId,
			identifierSetSha256[:],
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var certId server.Id
				server.Raise(result.Scan(&certId))
				certIds = append(certIds, certId)
			}
		})
	})
	errs := []error{}
	for _, certId := range certIds {
		if _, err := RequestTlsCertRevocation(ctx, &TlsCertRevocationRequest{
			NetworkId: networkId,
			CertId:    certId,
			Reason:    "alias:" + alias,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// What TLS-M6-03 calls when a certificate naming alias issues (reading 11), in the caller's transaction,
// which must be read committed: dns_label.certified_time is set, once, and the alias row takes its label's
// apex through §12.6's path, so the alias answers its label's records from that serial on. Returns the
// write, nil when the alias already answered. ErrDnsAliasNotLive for a name that is no live alias of the
// network: a retiring alias takes no certificate.
func DnsAliasCertifiedInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id, alias string, certifiedTime time.Time) (*DnsWriteResult, error) {
	if !dnsAliasLinked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	alias = dnsLabelText(root, alias)
	label, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	if err != nil {
		return nil, err
	}
	lockDnsZoneInTx(ctx, tx, root, networkId)
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE dns_label SET certified_time = COALESCE(certified_time, $5)
			WHERE root = $1 AND label = $2 AND network_id = $3 AND kind = $4 AND state = $6
		`,
		root,
		alias,
		networkId,
		DnsLabelKindAlias,
		certifiedTime.UTC(),
		DnsLabelStateLive,
	))
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("%w: %s", ErrDnsAliasNotLive, alias)
	}
	labelApex := label + "." + root
	want := dnsAliasRecordSet(root, alias, labelApex)
	for _, row := range getDnsZoneRowsInTx(ctx, tx, root, networkId) {
		if row.set.Key() == want.Key() && !row.withheld && len(row.set.Rdata) == 1 && row.set.Rdata[0] == labelApex {
			return nil, nil
		}
	}
	result, err := DnsWriteInTx(ctx, tx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{want}})
	if err != nil {
		return nil, err
	}
	touchDnsLabelMapInTx(ctx, tx, root, networkId, result.Serial)
	return result, nil
}

// DnsAliasCertifiedInTx in a transaction of its own, whose commit raises the serial.
func DnsAliasCertified(ctx context.Context, root string, networkId server.Id, alias string, certifiedTime time.Time) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsAliasCertifiedInTx(ctx, tx, root, networkId, alias, certifiedTime)
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

// The 90-day job (D-DNS-10, I-14): each alias retiring past its window at now tombstones, one transaction
// each, at most one batch; then the CAA a deletion's tombstone left at an alias's apex goes. Returns how
// many aliases tombstoned, and whether more wait. An alias whose root is restoring waits for the next pass.
func TombstoneRetiredDnsAliases(ctx context.Context, now time.Time) (tombstonedCount int, more bool, returnErr error) {
	if !dnsAliasLinked() {
		return 0, false, nil
	}
	settings := DefaultDnsAliasSettings()
	type retiredAlias struct {
		root      string
		name      string
		networkId server.Id
	}
	retired := []*retiredAlias{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT root, label, network_id FROM dns_label
				WHERE kind = $1 AND state = $2 AND network_id IS NOT NULL AND state_time < $3
				ORDER BY state_time, root, label
				LIMIT $4
			`,
			DnsLabelKindAlias,
			DnsLabelStateRetiring,
			now.Add(-settings.RetiringWindow).UTC(),
			settings.SweepBatchCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				alias := &retiredAlias{}
				server.Raise(result.Scan(&alias.root, &alias.name, &alias.networkId))
				retired = append(retired, alias)
			}
		})
	})
	errs := []error{}
	for _, alias := range retired {
		var tombstoned bool
		var aliasErr error
		err := dnsCatchRefusal(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				tombstoned, aliasErr = tombstoneDnsAliasInTx(ctx, tx, alias.root, alias.networkId, alias.name, now, settings)
				if aliasErr != nil {
					panic(dnsWriteRefusal{err: aliasErr})
				}
			}, server.TxReadCommitted)
		})
		switch {
		case err == nil && tombstoned:
			tombstonedCount += 1
		case err == nil:
		case errors.Is(err, dnsservice.ErrDnsRestoring):
			// the next pass, once the restore ends
		default:
			errs = append(errs, err)
		}
	}
	if err := removeOrphanedDnsAliasCaas(ctx, settings); err != nil {
		errs = append(errs, err)
	}
	return tombstonedCount, len(retired) == settings.SweepBatchCount, errors.Join(errs...)
}

// One alias's tombstone in tx (see TombstoneRetiredDnsAliases): every row at or below its apex, the
// network's alias row and <root>'s CAA alike, leaves in one journal write, and the name tombstones, never
// to be anyone's. False when the alias changed since the pass read it.
func tombstoneDnsAliasInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id, alias string, now time.Time, settings *DnsAliasSettings) (bool, error) {
	lockDnsLabelInTx(ctx, tx, networkId)
	lockDnsZoneInTx(ctx, tx, root, networkId)
	lockDnsZoneInTx(ctx, tx, root, server.Id{})
	due := false
	result, err := tx.Query(
		ctx,
		`
			SELECT state = $5 AND state_time < $6 FROM dns_label
			WHERE root = $1 AND label = $2 AND network_id = $3 AND kind = $4
			FOR UPDATE
		`,
		root,
		alias,
		networkId,
		DnsLabelKindAlias,
		DnsLabelStateRetiring,
		now.Add(-settings.RetiringWindow).UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&due))
		}
	})
	if !due {
		return false, nil
	}
	apex := alias + "." + root
	deletes := []dnsservice.DnsRecordKey{}
	for _, zoneNetworkId := range []server.Id{networkId, {}} {
		for _, row := range getDnsZoneRowsInTx(ctx, tx, root, zoneNetworkId) {
			if dnsOwnerAtOrBelow(row.set.Owner, apex) {
				deletes = append(deletes, row.set.Key())
			}
		}
	}
	var serial uint64
	if 0 < len(deletes) {
		serial, err = dnsCommitInTx(ctx, tx, &dnsservice.DnsZoneWrite{
			Root:      root,
			NetworkId: dnsSidecarId(networkId),
			Deletes:   deletes,
		}, now, nil)
		if err != nil {
			return false, err
		}
		server.AddTxPostCommit(tx, "dns_write:"+root, func() any {
			DnsWritePost(context.Background(), root, serial, networkId)
			return nil
		})
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE dns_label SET state = $4, state_time = $5, change_serial = GREATEST(change_serial, $6)
			WHERE root = $1 AND label = $2 AND network_id = $3
		`,
		root,
		alias,
		networkId,
		DnsLabelStateTombstoned,
		now.UTC(),
		dnsSerialColumn(serial),
	))
	if 0 < serial {
		touchDnsLabelMapInTx(ctx, tx, root, networkId, serial)
	}
	targetId := networkId
	AddAdminEventInTx(ctx, tx, &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  AdminEventActorSystem,
		Action:     AdminEventActionDnsAliasTombstone,
		TargetKind: AdminEventTargetDnsLabel,
		TargetId:   &targetId,
		Result:     AdminEventResultOk,
	})
	return true, nil
}

// Removes the retiring CAA <root> holds at the apex of an alias tombstoned before its 90 days ended, as a
// network's deletion tombstones its names at once, at most one batch, one write a root.
func removeOrphanedDnsAliasCaas(ctx context.Context, settings *DnsAliasSettings) error {
	rootDeletes := map[string][]dnsservice.DnsRecordKey{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_dns_record.root, network_dns_record.owner
				FROM network_dns_record
				INNER JOIN dns_label ON
					dns_label.root = network_dns_record.root AND
					network_dns_record.owner = dns_label.label || '.' || network_dns_record.root
				WHERE
					network_dns_record.network_id IS NULL AND
					network_dns_record.rrtype = $1 AND
					dns_label.kind = $2 AND
					dns_label.state = $3
				LIMIT $4
			`,
			int32(dns.TypeCAA),
			DnsLabelKindAlias,
			DnsLabelStateTombstoned,
			settings.SweepBatchCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var root string
				var owner string
				server.Raise(result.Scan(&root, &owner))
				rootDeletes[root] = append(rootDeletes[root], dnsservice.DnsRecordKey{Owner: owner, Type: dns.TypeCAA})
			}
		})
	})
	errs := []error{}
	for root, deletes := range rootDeletes {
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, Deletes: deletes}); err != nil && !errors.Is(err, dnsservice.ErrDnsRestoring) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
