package model

// §12.7's Team scope zones (DNS-M6-06; R-DNS-2, R-DNS-6, D-DNS-6; owner decisions 40, 58): each Team scope
// <scope>.<net>.<root> repeats a network zone's system set under its own apex, from the same function
// (DnsSystemRecords through DnsScopeRecords), with its own mx host and, from M8, its TLSA
// (_25._tcp.mx.<scope>, POST /dns/key-record's). A scope has no table of its own: its set is the network's
// rows at owners under the scope's apex, and what the set is a function of, its edge rows and mail, is read
// back from those rows, as a direct MX's addresses are (dns_direct_mx_model.go), so every reconcile writes
// them back as they stand.
//
//   - Scope labels come from the signed projection (§8; protos-bridge-row-fields #3's TeamScopeRow, which
//     TEAM defines at M6) through the process's DnsScopeLabelSource. Until a process installs one, no scope
//     zone is written there (ErrDnsScopeUnknown), user records take no scope owner, and the record API
//     refuses every name two labels below the zone's apex: the closed direction.
//   - A new scope's label is held to dnsservice.ValidateDnsScopeLabel: §21.6's screen with §12.5's reserved
//     block, none of §12.7's system hosts, no host the zone holds and no client name (§7.7), so a scope
//     never takes over a name the network already uses.
//   - In the network's own zone a scope label is refused as a user owner, and in a scope's zone only hosts
//     one label below its apex take user records, as dns:<host>.<scope> (dnsservice.DnsOwnerHost): the
//     record API (dns_records_model.go) validates each owner in the zone it falls in.
//   - A scope's system, edge and router rows are free of dns_entries (§12.7); its users' RRsets count as
//     the network's.
//   - A scope's @ takes no CAA canary and no custom host (DNS-M6-05, DNS-M6-02), since its CAA would govern
//     the scope's device certificates (RFC 8659 §3).
//
// Every writer takes the label's lock, then the zone's, as every zone writer does, and writes through
// §12.6's one path. Nothing here runs unless the binary links MG2 and MG5.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

var (
	// The scope is none of the network's Team scopes, or the process can read no scope labels.
	ErrDnsScopeUnknown = errors.New("dns scope unknown")
)

// Where a process reads a network's Team scope labels: the signed projection's (§8), from memory with no
// Postgres. ok false while the network's labels are not known, which reads as no scope.
type DnsScopeLabelSource func(ctx context.Context, networkId server.Id) (labels []string, ok bool)

var dnsScopeLabelSource atomic.Pointer[DnsScopeLabelSource]

// Both the administrative API and the standalone DNS block resolve scopes from verified rows.
// The first request for a network queues its load and cannot spend an unknown scope's authority.
func DnsScopeLabelSourceOf(cache *sidecar.AclGateCache) DnsScopeLabelSource {
	return func(ctx context.Context, networkId server.Id) ([]string, bool) {
		if cache == nil {
			return nil, false
		}
		id := sidecar.Id(networkId)
		snapshot, status := cache.State(id)
		if status != sidecar.AclGateLoaded || snapshot == nil || snapshot.Config == nil {
			cache.Changed(id)
			return nil, false
		}
		return snapshot.Config.TeamScopeLabels(), true
	}
}

// Installs where this process reads scope labels. nil removes it. Answers what puts the previous one back.
func SetDnsScopeLabelSource(source DnsScopeLabelSource) (restore func()) {
	var previous *DnsScopeLabelSource
	if source == nil {
		previous = dnsScopeLabelSource.Swap(nil)
	} else {
		previous = dnsScopeLabelSource.Swap(&source)
	}
	return func() {
		dnsScopeLabelSource.Store(previous)
	}
}

// The network's scope labels, sorted, and whether the process knows them.
func dnsNetworkScopeLabels(ctx context.Context, networkId server.Id) ([]string, bool) {
	source := dnsScopeLabelSource.Load()
	if source == nil {
		return nil, false
	}
	labels, ok := (*source)(ctx, networkId)
	if !ok {
		return nil, false
	}
	labels = slices.Clone(labels)
	slices.Sort(labels)
	return slices.Compact(labels), true
}

// What a scope zone's set is a function of.
type DnsScopeState struct {
	// The root, fully qualified.
	Root string
	// The network's zone, <label>.<root>, fully qualified.
	Apex string
	// The scope's label, directly below Apex.
	Scope string
	// *.<scope> A and AAAA: an edge grant on a host of the scope (plan-DNS-edge-record-trigger's rule, for
	// dns:<host>.<scope>).
	EdgeWildcard bool
	// <scope> A and AAAA: the scope's apex itself is published.
	EdgeApex bool
	// The scope's own mail (M8; DNS-M8-01): MX 10 mx.<scope>, mx.<scope>'s router rows and b.<scope>.
	Mail bool
	// What the network's mail set carries into the scope's: its report token and §12.7's policy records
	// (DNS-M8-01).
	MailPolicy *DnsZoneMailPolicy
}

// The zone state a scope's set is written from: a network zone's, at the scope's apex.
func (self *DnsScopeState) zoneState() *DnsZoneState {
	return &DnsZoneState{
		Root:         self.Root,
		Apex:         dnsservice.DnsScopeApex(self.Apex, self.Scope),
		EdgeWildcard: self.EdgeWildcard,
		EdgeApex:     self.EdgeApex,
		Mail:         self.Mail,
		MailPolicy:   self.MailPolicy,
	}
}

// §12.7's set for a Team scope (see the file header): DnsSystemRecords at the scope's apex, so a scope's
// set is a network's by construction. Refuses a scope label that is not one LDH label.
func DnsScopeRecords(state *DnsScopeState, ttl *dnsservice.DnsTtlSettings) ([]dnsservice.DnsRecordSet, error) {
	if !isDnsCtLabel(state.Scope) {
		return nil, fmt.Errorf("%w: %q is no scope label", dnsservice.ErrDnsScopeLabelRefused, state.Scope)
	}
	return DnsSystemRecords(state.zoneState(), ttl)
}

// The scope's state as its own rows hold it: edge rows at its wildcard and apex, and its mail by the
// router rows of its mx host.
func dnsScopeStateFromRows(state *DnsScopeState, rows []*dnsStoredRow) {
	scopeApex := dnsservice.DnsScopeApex(state.Apex, state.Scope)
	for _, row := range rows {
		switch {
		case row.set.Kind == dnsservice.DnsRecordKindEdge && row.set.Owner == "*."+scopeApex:
			state.EdgeWildcard = true
		case row.set.Kind == dnsservice.DnsRecordKindEdge && row.set.Owner == scopeApex:
			state.EdgeApex = true
		case row.set.Kind == dnsservice.DnsRecordKindRouter && row.set.Owner == dnsMailHostLabel+"."+scopeApex:
			state.Mail = true
		}
	}
}

// Whether rows hold the scope's system set: its apex's system MX, which every set writes.
func dnsScopeWritten(scopeApex string, rows []*dnsStoredRow) bool {
	return slices.ContainsFunc(rows, func(row *dnsStoredRow) bool {
		return row.set.Owner == scopeApex && row.set.Type == dns.TypeMX && row.set.Kind == dnsservice.DnsRecordKindSystem
	})
}

// One change to a scope zone.
type DnsScopeArgs struct {
	Root      string
	NetworkId server.Id
	// One of the network's scope labels, as the projection lists it.
	Scope string
	// Each nil keeps what the scope's rows hold; a new scope starts with each false.
	EdgeWildcard *bool
	EdgeApex     *bool
	Mail         *bool
	// The network's client names (§7.7), which a new scope's label may not be.
	ClientNames []string
	// nil takes dns.yml's.
	Ttl *dnsservice.DnsTtlSettings
}

// Writes a Team scope's set in the caller's transaction, which must be read committed (see the file
// header): a new scope's label held to §12.5's rules first, then its set from DnsScopeRecords, reconciled so
// one journal write a change and none when nothing changed. ErrDnsScopeUnknown for a scope the network's
// projection does not list, or with no scope source; a refused label wraps
// dnsservice.ErrDnsScopeLabelRefused. Returns the write, nil when nothing changed.
func DnsSetScopeRecordsInTx(ctx context.Context, tx server.PgTx, args *DnsScopeArgs) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	root := dnsLabelRoot(args.Root)
	scopes, ok := dnsNetworkScopeLabels(ctx, args.NetworkId)
	if !ok || !slices.Contains(scopes, args.Scope) {
		return nil, fmt.Errorf("%w: %q", ErrDnsScopeUnknown, args.Scope)
	}
	ttl := args.Ttl
	if ttl == nil {
		ttl = dnsZoneTtl()
	}
	// the label's lock, then the zone's, as every zone writer takes them
	label, err := EnsureNetworkLabelInTx(ctx, tx, args.NetworkId, root)
	if err != nil {
		return nil, err
	}
	lockDnsZoneInTx(ctx, tx, root, args.NetworkId)
	apex := label + "." + root
	scopeApex := dnsservice.DnsScopeApex(apex, args.Scope)
	stored := getDnsZoneRowsInTx(ctx, tx, root, args.NetworkId)
	if !dnsScopeWritten(scopeApex, stored) {
		owners := []string{}
		for _, row := range stored {
			owners = append(owners, row.set.Owner)
		}
		if err := dnsservice.ValidateDnsScopeLabel(args.Scope, &dnsservice.DnsScopeLabelContext{
			Hosts:       dnsservice.DnsZoneHostLabels(apex, owners),
			ClientNames: args.ClientNames,
			Facts:       &sidecar.ScreenFacts{VerifiedDomains: getNetworkVerifiedDomainsInTx(ctx, tx, args.NetworkId)},
		}); err != nil {
			return nil, err
		}
	}
	state := &DnsScopeState{Root: root, Apex: apex, Scope: args.Scope}
	dnsScopeStateFromRows(state, stored)
	if args.EdgeWildcard != nil {
		state.EdgeWildcard = *args.EdgeWildcard
	}
	if args.EdgeApex != nil {
		state.EdgeApex = *args.EdgeApex
	}
	if args.Mail != nil {
		state.Mail = *args.Mail
	}
	state.MailPolicy = getDnsZoneMailPolicyInTx(ctx, tx, root, args.NetworkId)
	desired, err := DnsScopeRecords(state, ttl)
	if err != nil {
		return nil, err
	}
	result, err := reconcileDnsSystemRowsInTx(ctx, tx, root, args.NetworkId, dnsZoneSystemKeys(scopeApex), desired)
	if err != nil {
		return nil, err
	}
	if result != nil && 0 < result.Serial {
		touchDnsLabelMapInTx(ctx, tx, root, args.NetworkId, result.Serial)
	}
	return result, nil
}

// DnsSetScopeRecordsInTx in a transaction of its own.
func DnsSetScopeRecords(ctx context.Context, args *DnsScopeArgs) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsSetScopeRecordsInTx(ctx, tx, args)
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

// Withdraws a Team scope in the caller's transaction, which must be read committed: every row at or below
// the scope's apex, its users' included, in one write, as the scope leaves the projection. A scope with no
// rows writes nothing. Returns the write, nil when nothing went.
func DnsRemoveScopeRecordsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, scope string) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	if !isDnsCtLabel(scope) {
		return nil, fmt.Errorf("%w: %q is no scope label", dnsservice.ErrDnsScopeLabelRefused, scope)
	}
	root = dnsLabelRoot(root)
	label, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	if err != nil {
		return nil, err
	}
	lockDnsZoneInTx(ctx, tx, root, networkId)
	scopeApex := dnsservice.DnsScopeApex(label+"."+root, scope)
	deletes := []dnsservice.DnsRecordKey{}
	for _, row := range getDnsZoneRowsInTx(ctx, tx, root, networkId) {
		if dnsOwnerAtOrBelow(row.set.Owner, scopeApex) {
			deletes = append(deletes, row.set.Key())
		}
	}
	if len(deletes) == 0 {
		return nil, nil
	}
	result, err := DnsWriteInTx(ctx, tx, &DnsWriteArgs{Root: root, NetworkId: networkId, Deletes: deletes})
	if err != nil {
		return nil, err
	}
	touchDnsLabelMapInTx(ctx, tx, root, networkId, result.Serial)
	return result, nil
}

// Which zone a user owner of the network falls in, for the record API: the scope's, when the owner is at or
// below one of scopes, else the network's own, apex being <label>.<root>. The returned context validates the
// owner by §12.7's rules in that zone.
func dnsUserRecordContextFor(base *dnsservice.DnsUserRecordContext, owner string, scopes []string) *dnsservice.DnsUserRecordContext {
	scope := dnsservice.DnsOwnerScope(base.Apex, owner, scopes)
	if scope == "" {
		return base
	}
	scoped := *base
	scoped.Apex = dnsservice.DnsScopeApex(base.Apex, scope)
	scoped.Scope = scope
	scoped.ScopeLabels = nil
	return &scoped
}

// The labels of owner a user write screens (§21.6's ScreenHost): those below the zone it falls in, * and
// underscore labels left out, so a scope's own label, which ScreenHost refuses as a host, is not screened as
// one of its hosts.
func dnsScreenedOwnerLabels(context *dnsservice.DnsUserRecordContext, owner string) []string {
	labels := []string{}
	rest, ok := strings.CutSuffix(owner, "."+context.Apex)
	if !ok {
		return labels
	}
	for _, label := range dns.SplitDomainName(rest + ".") {
		if label == "*" || strings.HasPrefix(label, "_") {
			continue
		}
		labels = append(labels, label)
	}
	return labels
}
