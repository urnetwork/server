package model

// §12.8's custom domains (DNS-M6-02, DNS-M7-03; R-DNS-3, D-DNS-9, I-8, R-CTX-20; DESIGN-DETAILS 17.7):
// network_dns_custom_host's rows and the routes that make and remove them, Team only.
//
//   - A custom host is <host>.<domain>, or an apex <domain>, of one of the network's verified domains
//     (§7.6), mapped to a host owner of the network's zone, <host>.<label>.<root>: never the zone's apex,
//     an alias's or a scope's, whose CAA would govern the zone's device certificates (RFC 8659 §3), and
//     never * or a system owner.
//   - Strong mode is the default and every apex's: the row commits pending, unmapped, with
//     effective_caa_chain_ok false, and only certs' CAA walk and probe make it active, through
//     DnsCustomHostResultInTx (TLS-M6-04). Weak mode, a CNAME into the root, is active at once and taken
//     only with §2.8 EX1's disclosure accepted in the request, which an admin event records.
//   - DnsCustomHostResultInTx takes certs' verdict. A pending host passing goes active; one failing, a walk
//     that met a URnetwork zone or a failed probe, is refused, its row removed, with
//     custom_host_caa_operator_alias in an admin event. On §13.8's later schedule a failure clears
//     effective_caa_chain_ok and records EX1's exception in an admin event that alarms the network's Owners
//     and Admins through the network authority notice, the host staying mapped; a pass sets it again.
//   - DnsCustomHostActiveRows is the map's source: the active TLS hosts, which tlsedge routes by SNI to
//     tls/<port>/<owner> (TLS-M6-02) and DNS-M6-03 versions for each lb interface.
//   - A strict-mode MX host (§15.4, owner decision 98; DNS-M7-03) is a row whose owner is the network's
//     mail host, mx.<label>.<root>: the customer's own <mx>.<domain>, with its MX, TLSA and addresses in
//     its own signed zone, mapped here for mxedge (§2.8 mx-3). dns publishes nothing for it, since only
//     that path carries strict mode's claim; it is active at once, never in the TLS map, and
//     DnsStrictMxHostRows lists it.
//   - A custom host counts against no dns_entries (§20.7's only limit here is Team), and a network maps at
//     most SocketsSettings.CertifiedNameMaxCount custom hosts to one owner, since a listener lists every
//     custom host of its owner it is certified for (DESIGN-DETAILS 17.2.10).
//   - Each removal, the route's and a domain's (DnsRemoveDomainCustomHostsInTx, which the domain's removal
//     and its daily drop call), is one transaction a model function can join, where TLS-M6-05 adds 17.2.12's
//     move of the host's certificate rows.
//
// The map's versions, readiness and map-stale are DNS-M6-03's (dns_custom_host_map_model.go): each change to
// the active rows here raises the map's version in its transaction, and a row's map_version and ready_time are
// written there. Nothing here runs unless the binary links MG2, MG5 and MG6.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
)

// The admin events of custom hosts (§7.11): the network is the target, and no host is said (I-13).
const (
	AdminEventActionDnsCustomHostAdd    = "dns.custom_host_add"
	AdminEventActionDnsCustomHostRemove = "dns.custom_host_remove"
	// certs' verdict on a host's CAA (§12.8 step 3)
	AdminEventActionDnsCustomHostCaa = "dns.custom_host_caa"
	AdminEventTargetDnsCustomHost    = "dns_custom_host"
)

// The results a custom host's events carry beside ok.
const (
	// a weak-mode host, with §2.8 EX1's exception disclosed and accepted
	AdminEventResultDnsCustomHostWeak = "weak_mode"
	// a pending host's walk met a URnetwork zone, or its probe failed: refused (§12.8 step 3)
	AdminEventResultDnsCustomHostOperatorAlias = "custom_host_caa_operator_alias"
	// an active host's later check failed: exposed as a weak-mode host is (§2.8 EX1)
	AdminEventResultDnsCustomHostExposed = "custom_host_caa_exposed"
)

// The network authority notice's word for a custom host's CAA exception: a URnetwork account could be
// admitted to issue for it.
const SecurityNetworkAuthorityCustomHostCaa = "custom_host_caa"

var (
	// The host is no custom host of the network.
	ErrDnsCustomHostUnknown = errors.New("dns custom host unknown")
)

// Whether the binary links what custom hosts read.
func dnsCustomHostLinked() bool {
	return server.SidecarMg2Linked() && server.SidecarMg5Linked() && server.SidecarMg6Linked()
}

// The Team check custom hosts and a direct MX ask (R-DNS-3), which tests replace.
var dnsRequireTeamTierInTx = RequireTeamTierInTx

// The most custom hosts one owner maps from: SocketsSettings.CertifiedNameMaxCount.
func dnsCustomHostOwnerMaxCount() int {
	return socketsservice.DefaultSocketsSettings().CertifiedNameMaxCount
}

// One row of network_dns_custom_host.
type DnsCustomHostRow struct {
	// lowercase, no final dot
	Host string
	// the root, canonical
	Root      string
	NetworkId server.Id
	// the host owner, canonical
	Owner               string
	State               string
	CaaMode             string
	EffectiveCaaChainOk bool
	MapVersion          uint64
	ReadyTime           *time.Time
	CreateTime          time.Time
}

// Whether the row is a strict-mode MX host: its owner is the network's mail host.
func (self *DnsCustomHostRow) StrictMx() bool {
	return strings.HasPrefix(self.Owner, dnsMailHostLabel+".")
}

// The row as GET /network/dns and the routes show it.
func (self *DnsCustomHostRow) answer() *sidecar.DnsCustomHost {
	return &sidecar.DnsCustomHost{
		Host:                self.Host,
		Owner:               self.Owner,
		State:               self.State,
		CaaMode:             self.CaaMode,
		EffectiveCaaChainOk: self.EffectiveCaaChainOk,
		Ready:               self.ReadyTime != nil,
	}
}

const dnsCustomHostColumns = `host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, map_version, ready_time, create_time`

func scanDnsCustomHostRow(result server.PgResult) *DnsCustomHostRow {
	row := &DnsCustomHostRow{}
	var mapVersion int64
	server.Raise(result.Scan(
		&row.Host,
		&row.Root,
		&row.NetworkId,
		&row.Owner,
		&row.State,
		&row.CaaMode,
		&row.EffectiveCaaChainOk,
		&mapVersion,
		&row.ReadyTime,
		&row.CreateTime,
	))
	row.MapVersion = dnsSerialFromColumn(mapVersion)
	return row
}

// The rows a query's where clause selects, in host order.
func getDnsCustomHostRowsInTx(ctx context.Context, query server.PgCanQuery, where string, args ...any) []*DnsCustomHostRow {
	rows := []*DnsCustomHostRow{}
	result, err := query.Query(ctx, `SELECT `+dnsCustomHostColumns+` FROM network_dns_custom_host WHERE `+where+` ORDER BY host`, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			rows = append(rows, scanDnsCustomHostRow(result))
		}
	})
	return rows
}

// The network's custom hosts, strict-mode MX hosts included, in host order.
func getNetworkDnsCustomHostsInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id) []*DnsCustomHostRow {
	return getDnsCustomHostRowsInTx(ctx, query, `network_id = $1`, networkId)
}

// Every active custom host for the TLS edge, strict-mode MX hosts left out, in host order: the map's
// source (§13.4; TLS-M6-02, DNS-M6-03). Empty in a binary that does not link MG6.
func DnsCustomHostActiveRows(ctx context.Context) []*DnsCustomHostRow {
	rows := []*DnsCustomHostRow{}
	if !dnsCustomHostLinked() {
		return rows
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows = DnsCustomHostActiveRowsInTx(ctx, conn)
	})
	return rows
}

// DnsCustomHostActiveRows in the caller's transaction.
func DnsCustomHostActiveRowsInTx(ctx context.Context, query server.PgCanQuery) []*DnsCustomHostRow {
	rows := []*DnsCustomHostRow{}
	for _, row := range getDnsCustomHostRowsInTx(ctx, query, `state = $1`, sidecar.DnsCustomHostStateActive) {
		if !row.StrictMx() {
			rows = append(rows, row)
		}
	}
	return rows
}

// The network's strict-mode MX hosts, which mxedge maps by SNI to the network (§12.8 step 4, §2.8 mx-3),
// in host order. Empty in a binary that does not link MG6.
func DnsStrictMxHostRows(ctx context.Context, networkId server.Id) []*DnsCustomHostRow {
	rows := []*DnsCustomHostRow{}
	if !dnsCustomHostLinked() {
		return rows
	}
	server.Db(ctx, func(conn server.PgConn) {
		for _, row := range getNetworkDnsCustomHostsInTx(ctx, conn, networkId) {
			if row.StrictMx() {
				rows = append(rows, row)
			}
		}
	})
	return rows
}

// One add or remove as a route takes it.
type DnsCustomHostRequest struct {
	Root      string
	NetworkId server.Id
	Add       *sidecar.DnsCustomHostAddRequest
	Remove    *sidecar.DnsCustomHostRemoveRequest
	// The admin event's actor (§7.11).
	ActorKind string
	ActorId   *server.Id
	RequestId *server.Id
}

// The verified domain of the network that host is, or is under, the longest one, "" for none.
func getNetworkDnsCustomHostDomainInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, host string) string {
	domain := ""
	result, err := query.Query(
		ctx,
		`
			SELECT domain FROM network_domain
			WHERE network_id = $1 AND verify_time IS NOT NULL AND ($2 = domain OR $2 LIKE '%.' || domain)
			ORDER BY length(domain) DESC
			LIMIT 1
		`,
		networkId,
		host,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&domain))
		}
	})
	return domain
}

// The host label of owner, <host>.<label>.<root>, if owner is a host owner of the label's zone a custom host
// may map to: one lowercase LDH label below the apex, not *, not an underscore label, not a word-number
// label, which burners keep, and not b, mail or mta-sts, §12.7's system hosts. mx is the network's mail
// host, which only a strict-mode MX host maps to.
func dnsCustomHostOwnerLabel(owner string, apex string) (string, error) {
	rest, ok := strings.CutSuffix(owner, "."+apex)
	if !ok || rest == "" || strings.Contains(rest, ".") {
		return "", fmt.Errorf("the owner %s is no host of the zone %s", owner, apex)
	}
	if !isDnsCtLabel(rest) || sidecar.IsWordNumberDnsLabel(rest) {
		return "", fmt.Errorf("the owner %s is no host a custom host maps to", owner)
	}
	switch rest {
	case dnsMailEnvelopeLabel, "mail", "mta-sts":
		return "", fmt.Errorf("%s is a system owner", rest)
	}
	return rest, nil
}

// §12.8's custom-host-add (see the file header): the host as it stands, or a *sidecar.DnsApiError with
// nothing written. Adding a host the network already maps to the same owner in the same mode answers it
// again.
func AddDnsCustomHost(ctx context.Context, request *DnsCustomHostRequest) (*sidecar.DnsCustomHostAnswer, error) {
	if !dnsCustomHostLinked() {
		return nil, dnsRecordsUnavailable("the custom-host tables are not linked")
	}
	if request == nil || request.NetworkId == (server.Id{}) || request.Add == nil {
		return nil, dnsRecordsInvalid("no body")
	}
	root := dnsLabelRoot(request.Root)
	add := request.Add
	host, err := NormalizeNetworkDomain(add.Host)
	if err != nil {
		return nil, dnsRecordsInvalid("%v", err)
	}
	trimmedRoot := strings.TrimSuffix(root, ".")
	if host == trimmedRoot || strings.HasSuffix(host, "."+trimmedRoot) {
		return nil, dnsRecordsInvalid("%s is under the root, where no custom host is", host)
	}
	mode := add.Mode
	if mode == "" {
		mode = sidecar.DnsCustomHostModeStrong
	}
	if mode != sidecar.DnsCustomHostModeStrong && mode != sidecar.DnsCustomHostModeWeak {
		return nil, dnsRecordsInvalid("mode %q is neither strong nor weak", add.Mode)
	}
	if mode == sidecar.DnsCustomHostModeWeak && !add.WeakDisclosureAccepted {
		return nil, dnsRecordsInvalid("weak mode is taken only with its disclosure accepted (§2.8 EX1)")
	}
	owner := sidecar.CanonicalDnsRecordName(add.Owner)
	var answer *sidecar.DnsCustomHostAnswer
	var returnErr error
	err = dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			answer, returnErr = addDnsCustomHostInTx(ctx, tx, root, host, owner, mode, request)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, dnsRecordsApiError(err)
	}
	return answer, nil
}

func addDnsCustomHostInTx(ctx context.Context, tx server.PgTx, root string, host string, owner string, mode string, request *DnsCustomHostRequest) (*sidecar.DnsCustomHostAnswer, error) {
	networkId := request.NetworkId
	if err := dnsRequireTeamTierInTx(ctx, tx, networkId); err != nil {
		return nil, err
	}
	label, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	if err != nil {
		return nil, err
	}
	apex := label + "." + root
	hostLabel, err := dnsCustomHostOwnerLabel(owner, apex)
	if err != nil {
		return nil, dnsRecordsInvalid("%v", err)
	}
	// a Team scope's @ is no host owner: its CAA would govern the scope's device certificates (DNS-M6-06)
	if scopeLabels, _ := dnsNetworkScopeLabels(ctx, networkId); slices.Contains(scopeLabels, hostLabel) ||
		dnsScopeWritten(owner, getDnsZoneRowsInTx(ctx, tx, root, networkId)) {
		return nil, dnsRecordsInvalid("the owner %s is a Team scope's apex", owner)
	}
	strictMx := hostLabel == dnsMailHostLabel
	domain := getNetworkDnsCustomHostDomainInTx(ctx, tx, networkId, host)
	if domain == "" {
		return nil, dnsRecordsRefused("%s is under no verified domain of the network (§7.6)", host)
	}
	if mode == sidecar.DnsCustomHostModeWeak && (host == domain || strictMx) {
		return nil, dnsRecordsInvalid("weak mode is a CNAME, which no apex and no mail host takes")
	}
	// the network's custom hosts change one at a time, so the owner's count holds
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('dns_custom_host:' || $1::uuid::text, 0))`,
		networkId,
	))
	if existing := getDnsCustomHostRowsInTx(ctx, tx, `host = $1`, host); 0 < len(existing) {
		row := existing[0]
		if row.NetworkId == networkId && row.Owner == owner && row.CaaMode == mode {
			return &sidecar.DnsCustomHostAnswer{CustomHost: row.answer()}, nil
		}
		return nil, dnsRecordsError(http.StatusConflict, sidecar.DnsApiCodeRefused, "%s is mapped already; remove it first", host)
	}
	ownerCount := 0
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT count(*) FROM network_dns_custom_host WHERE network_id = $1 AND owner = $2`,
		networkId,
		owner,
	).Scan(&ownerCount))
	if limit := dnsCustomHostOwnerMaxCount(); limit <= ownerCount {
		return nil, dnsRecordsError(http.StatusConflict, sidecar.DnsApiCodeRefused, "an owner maps at most %d custom hosts (certified_name_max_count)", limit)
	}
	now := server.NowUtc()
	state := sidecar.DnsCustomHostStatePending
	if mode == sidecar.DnsCustomHostModeWeak || strictMx {
		// weak mode is active at once (§12.8 step 2); a strict-mode MX host has nothing of dns's to check
		state = sidecar.DnsCustomHostStateActive
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, create_time)
			VALUES ($1, $2, $3, $4, $5, $6, false, $7)
		`,
		host,
		root,
		networkId,
		owner,
		state,
		mode,
		now,
	))
	var mapVersion uint64
	if state == sidecar.DnsCustomHostStateActive && !strictMx {
		// a weak-mode host enters the map at once (DNS-M6-03), before the admin event's chain lock
		version, err := DnsRaiseCustomHostMapInTx(ctx, tx, root)
		if err != nil {
			return nil, err
		}
		mapVersion = version
	}
	result := AdminEventResultOk
	if mode == sidecar.DnsCustomHostModeWeak {
		result = AdminEventResultDnsCustomHostWeak
	}
	targetId := networkId
	AddAdminEventInTx(ctx, tx, &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  dnsAdminEventActorKind(request.ActorKind),
		ActorId:    request.ActorId,
		Action:     AdminEventActionDnsCustomHostAdd,
		TargetKind: AdminEventTargetDnsCustomHost,
		TargetId:   &targetId,
		Result:     result,
		RequestId:  request.RequestId,
	})
	row := &DnsCustomHostRow{Host: host, Root: root, NetworkId: networkId, Owner: owner, State: state, CaaMode: mode, MapVersion: mapVersion, CreateTime: now}
	answer := &sidecar.DnsCustomHostAnswer{CustomHost: row.answer()}
	if mode == sidecar.DnsCustomHostModeWeak {
		answer.Disclosure = sidecar.DnsCustomHostWeakDisclosure
	}
	return answer, nil
}

// §12.8's custom-host-remove: whether a row went, or a *sidecar.DnsApiError.
func RemoveDnsCustomHost(ctx context.Context, request *DnsCustomHostRequest) (*sidecar.DnsCustomHostAnswer, error) {
	if !dnsCustomHostLinked() {
		return nil, dnsRecordsUnavailable("the custom-host tables are not linked")
	}
	if request == nil || request.NetworkId == (server.Id{}) || request.Remove == nil {
		return nil, dnsRecordsInvalid("no body")
	}
	host, err := NormalizeNetworkDomain(request.Remove.Host)
	if err != nil {
		return nil, dnsRecordsInvalid("%v", err)
	}
	answer := &sidecar.DnsCustomHostAnswer{}
	server.Tx(ctx, func(tx server.PgTx) {
		answer.Removed = DnsRemoveCustomHostInTx(ctx, tx, request.NetworkId, host)
		if answer.Removed {
			targetId := request.NetworkId
			AddAdminEventInTx(ctx, tx, &AdminEvent{
				NetworkId:  request.NetworkId,
				ActorKind:  dnsAdminEventActorKind(request.ActorKind),
				ActorId:    request.ActorId,
				Action:     AdminEventActionDnsCustomHostRemove,
				TargetKind: AdminEventTargetDnsCustomHost,
				TargetId:   &targetId,
				Result:     AdminEventResultOk,
				RequestId:  request.RequestId,
			})
		}
	}, server.TxReadCommitted)
	return answer, nil
}

// Removes the network's custom host in the caller's transaction, a strict-mode MX host included, and
// answers whether a row went. The removal's one transaction, which TLS-M6-05's move of the host's
// certificate rows joins (DESIGN-DETAILS 17.2.12). An active TLS host leaving raises the map's version
// (DNS-M6-03), so FS-35's route goes at the next version; call it before the transaction's admin events.
func DnsRemoveCustomHostInTx(ctx context.Context, tx server.PgTx, networkId server.Id, host string) bool {
	if !dnsCustomHostLinked() {
		return false
	}
	return 0 < deleteDnsCustomHostsInTx(ctx, tx, `host = $1 AND network_id = $2`, strings.ToLower(strings.TrimSuffix(host, ".")), networkId)
}

// Removes every custom host of the network at or under domain in the caller's transaction, as the
// domain's removal and its daily drop do (§7.6): no host outlives the verification it stood on. Answers
// how many went, raising the map's version when an active TLS host was among them, as
// DnsRemoveCustomHostInTx does.
func DnsRemoveDomainCustomHostsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, domain string) int {
	if !dnsCustomHostLinked() {
		return 0
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	return deleteDnsCustomHostsInTx(ctx, tx, `network_id = $1 AND (host = $2 OR host LIKE '%.' || $2)`, networkId, domain)
}

// Deletes the rows where selects, raising each root's map that loses an active TLS host. Answers how many
// went.
func deleteDnsCustomHostsInTx(ctx context.Context, tx server.PgTx, where string, args ...any) int {
	removedCount := 0
	raiseRoots := []string{}
	result, err := tx.Query(ctx, `DELETE FROM network_dns_custom_host WHERE `+where+` RETURNING root, owner, state`, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var root string
			var owner string
			var state string
			server.Raise(result.Scan(&root, &owner, &state))
			removedCount += 1
			row := &DnsCustomHostRow{Owner: owner}
			if state == sidecar.DnsCustomHostStateActive && !row.StrictMx() && !slices.Contains(raiseRoots, root) {
				raiseRoots = append(raiseRoots, root)
			}
		}
	})
	slices.Sort(raiseRoots)
	for _, root := range raiseRoots {
		_, err := DnsRaiseCustomHostMapInTx(ctx, tx, root)
		server.Raise(err)
	}
	return removedCount
}

// What certs' walk and probe found for a custom host (§12.8 step 3; TLS-M6-04).
type DnsCustomHostResult struct {
	Host string
	// the walk reached the customer's own pin and the probe saw no nonce
	Pass bool
}

// What DnsCustomHostResultInTx did.
type DnsCustomHostOutcome string

const (
	// a pending host went active
	DnsCustomHostOutcomeActivated DnsCustomHostOutcome = "activated"
	// a pending host was refused, its row removed
	DnsCustomHostOutcomeRefused DnsCustomHostOutcome = "refused"
	// an active host's check passed
	DnsCustomHostOutcomeChecked DnsCustomHostOutcome = "checked"
	// an active host's check failed: still mapped, exposed as a weak-mode host is
	DnsCustomHostOutcomeExposed DnsCustomHostOutcome = "exposed"
)

// Applies certs' verdict for a strong-mode host in the caller's transaction (see the file header): at setup,
// while the host is pending, a pass activates it and a failure refuses it with
// custom_host_caa_operator_alias; later, while it is active, a failure clears effective_caa_chain_ok and
// records EX1's exception in an admin event and the network authority notice, the host staying mapped, and a
// pass sets the flag again. ErrDnsCustomHostUnknown for a host no row holds, removed since certs began, for a
// weak-mode or strict-mode MX host, which certs checks neither way, and for a host a downgrade withholds.
func DnsCustomHostResultInTx(ctx context.Context, tx server.PgTx, result *DnsCustomHostResult) (DnsCustomHostOutcome, error) {
	if !dnsCustomHostLinked() {
		return "", ErrDnsUnlinked
	}
	host := strings.ToLower(strings.TrimSuffix(result.Host, "."))
	var row *DnsCustomHostRow
	selected, err := tx.Query(ctx, `SELECT `+dnsCustomHostColumns+` FROM network_dns_custom_host WHERE host = $1 FOR UPDATE`, host)
	server.WithPgResult(selected, err, func() {
		if selected.Next() {
			row = scanDnsCustomHostRow(selected)
		}
	})
	// a withheld host is no certs' to judge until a release makes it active again (QD-840)
	if row == nil || row.CaaMode != sidecar.DnsCustomHostModeStrong || row.StrictMx() || row.State == DnsCustomHostStateWithheld {
		return "", fmt.Errorf("%w: %s", ErrDnsCustomHostUnknown, host)
	}
	targetId := row.NetworkId
	event := &AdminEvent{
		NetworkId:  row.NetworkId,
		ActorKind:  AdminEventActorSystem,
		Action:     AdminEventActionDnsCustomHostCaa,
		TargetKind: AdminEventTargetDnsCustomHost,
		TargetId:   &targetId,
		Result:     AdminEventResultOk,
	}
	var outcome DnsCustomHostOutcome
	switch {
	case row.State == sidecar.DnsCustomHostStatePending && result.Pass:
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_dns_custom_host SET state = $2, effective_caa_chain_ok = true WHERE host = $1`,
			host,
			sidecar.DnsCustomHostStateActive,
		))
		outcome = DnsCustomHostOutcomeActivated
	case row.State == sidecar.DnsCustomHostStatePending:
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_dns_custom_host WHERE host = $1`, host))
		event.Result = AdminEventResultDnsCustomHostOperatorAlias
		outcome = DnsCustomHostOutcomeRefused
	case result.Pass:
		if row.EffectiveCaaChainOk {
			return DnsCustomHostOutcomeChecked, nil
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_dns_custom_host SET effective_caa_chain_ok = true WHERE host = $1`, host))
		outcome = DnsCustomHostOutcomeChecked
	default:
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_dns_custom_host SET effective_caa_chain_ok = false WHERE host = $1`, host))
		event.Result = AdminEventResultDnsCustomHostExposed
		outcome = DnsCustomHostOutcomeExposed
		// the alarm: every Owner and Admin hears that a URnetwork account could be admitted for the host
		oweSecurityNetworkAuthorityNoticeInTx(ctx, tx, row.NetworkId, &SecurityNetworkAuthorityChange{
			ChangeTime: server.NowUtc(),
			Action:     SecurityNetworkAuthorityCustomHostCaa,
		})
	}
	if outcome == DnsCustomHostOutcomeActivated {
		// the host enters the map (DNS-M6-03), before the admin event's chain lock
		if _, err := DnsRaiseCustomHostMapInTx(ctx, tx, row.Root); err != nil {
			return "", err
		}
	}
	AddAdminEventInTx(ctx, tx, event)
	return outcome, nil
}

// DnsCustomHostResultInTx in a transaction of its own.
func DnsCustomHostResultApply(ctx context.Context, result *DnsCustomHostResult) (outcome DnsCustomHostOutcome, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		outcome, returnErr = DnsCustomHostResultInTx(ctx, tx, result)
	}, server.TxReadCommitted)
	return
}

// GET /network/dns's custom hosts and verified domains, the latter with the challenge target their one-time
// _acme-challenge CNAME names (QD-175). Empty in a binary that does not link MG6.
func getNetworkDnsCustomInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) ([]sidecar.DnsCustomHost, []sidecar.DnsCustomDomain) {
	hosts := []sidecar.DnsCustomHost{}
	domains := []sidecar.DnsCustomDomain{}
	if !server.SidecarMg6Linked() {
		return hosts, domains
	}
	readiness := map[string]*DnsCustomHostReadinessRow{}
	for _, row := range getDnsCustomHostReadinessInTx(ctx, query, root, networkId, server.NowUtc()) {
		readiness[row.Host] = row
	}
	for _, row := range getNetworkDnsCustomHostsInTx(ctx, query, networkId) {
		answer := row.answer()
		// a host not yet ready names the interfaces not current for it (DNS-M6-03, §13.4)
		if hostReadiness, ok := readiness[row.Host]; ok && !answer.Ready {
			answer.PendingInterfaces = hostReadiness.PendingInterfaces
		}
		hosts = append(hosts, *answer)
	}
	result, err := query.Query(
		ctx,
		`
			SELECT domain, acme_token FROM network_domain
			WHERE network_id = $1 AND verify_time IS NOT NULL AND acme_token IS NOT NULL
			ORDER BY domain
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var domain string
			var acmeToken string
			server.Raise(result.Scan(&domain, &acmeToken))
			domains = append(domains, sidecar.DnsCustomDomain{
				Domain:              domain,
				AcmeChallengeTarget: DnsAcmeChallengeTarget(root, acmeToken),
			})
		}
	})
	return hosts, domains
}

// The label below <root> a custom domain's delegated challenges sit under (QD-175).
const dnsAcmeChallengeLabel = "acme"

// <acme_token>.acme.<root> with no final dot, where certs writes a custom domain's DNS-01 values.
func DnsAcmeChallengeTarget(root string, acmeToken string) string {
	return acmeToken + "." + dnsAcmeChallengeLabel + "." + strings.TrimSuffix(dns.CanonicalName(root), ".")
}
