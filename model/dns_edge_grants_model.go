package model

// §12.7's edge address records follow the network's edge grants (owner decision 335; plan-DNS-edge-record-trigger,
// QD-130): * answers the healthy lb interfaces while the network's verified state publishes any host of its zone but
// @, or the system publish, and @ while it publishes @; and each Team scope's * while the state publishes a host of
// that scope. DnsSyncEdgeRecordsInTx (dns_zone_model.go) writes the zone's two flags and their rows, and
// dnsSetScopeRecordsInTx (dns_scope_model.go) a scope's set; this file decides both and calls them, which DNS-M5-11
// and DNS-M6-06 left to the callers.
//
//	grants    a state's published names (sidecar.AclState.PublishedNames at now, §21.6's list, by the agreement
//	          rule PublishGrant reads): a one-label host or a glob sets *, @ sets @, and the system publish's host,
//	          mta-sts, sets * only while the network has a mail_network row, since mta-sts.<net>.<root> has no row
//	          of its own and resolves only through * (§13.9). A scope host, <host>.<scope>, a glob host included,
//	          sets its scope's * when the state lists the scope, and a bare * sets every scope's too, since dns:*
//	          takes every host (§8.4). No grant spells a scope's @, so a scope's apex answers no address
//	scopes    each Team scope the state lists (sidecar.ConfigState.TeamScopeLabels: the verified projection, never
//	          a hostname or a grant, §12.5) repeats the zone's set under <scope>.<net>.<root> (DnsScopeRecords)
//	          once the zone exists: its * by the grants, its mail the zone's own flag read under the zone's lock,
//	          so with mail on it answers MX 10 mx.<scope>, mx.<scope>'s router rows, b.<scope> and, for a verified
//	          network, _mta-sts. A scope whose first set §12.5's rules refuse (a host the zone holds, the screen) is
//	          counted and left unwritten. Scope labels never leave a valid projection, so nothing here withdraws one
//	aliases   a certified alias answers its label's rows with its apex rewritten (dnsservice's zone_alias.go): its
//	          * and @ are the label's, and each scope's set below it too, so an alias zone follows these grants with
//	          no write of its own; one claimed and not yet certified answers only its own challenges (reading 11)
//	api       after each accepted POST /network/config/delta or /network/config/projection, once its entry's PUT
//	          has committed, api reads the network's state as its verifier holds it, at or past the entry, and syncs
//	          (SyncDnsEdgeRecordsAtTip), best effort: the entry stands whatever the sync does, a failure is logged,
//	          and the pass mends it
//	pass      every hour taskworker walks the networks with a log in id order, each one's published names and
//	          scope labels from the process's DnsEdgePublishedSource (taskworker's gate cache, whose states QD-770's
//	          published names read), and syncs each (ReconcileDnsEdgeRecords): a grant that expired, which no entry
//	          marks, loses its records, and a sync a crash lost between an entry's PUT and its write is made
//	sync      a zone whose flags and scope sets already hold the grants is not written, and no zone is made for a
//	          network with no grant: only a first publish, a scope host's included, draws a label (§12.5).
//	          Otherwise one DnsSyncEdgeRecords for the zone's flags and one write for each scope set that differs,
//	          each one journal row a transition in a transaction of its own, since §7.9 commits by a conditional
//	          PUT and has no transaction to join
//
// Two syncs of one network may race, each from the state it read, so the later commit may carry the older state;
// the next entry's sync or the hourly pass brings it back, the lag §12.7 accepts. A scope's mail is read under the
// zone's lock where its set is written, so a mail toggle is never written back over. Whether a listener is up never
// shows: §9.4's FirstListen and LastUnlisten have no DNS consumer.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The system publish's host (§13.9): tls/443/mta-sts's.
const dnsEdgeSystemPublishHost = "mta-sts"

// This process cannot read the networks' published states for the pass: no source is installed.
var ErrDnsEdgePublishedUnavailable = errors.New("dns edge published states are unavailable in this process")

// Which of a network zone's * and @ answer the healthy lb interfaces.
type DnsEdgeGrants struct {
	Wildcard bool
	Apex     bool
}

// The grants of the hosts a network's state publishes (the file header's grants row). mailNetwork answers whether
// the network has a mail_network row, asked only where the system publish alone would set *; nil answers no.
func DnsEdgeGrantsOfHosts(hosts []string, mailNetwork func() bool) DnsEdgeGrants {
	grants := DnsEdgeGrants{}
	system := false
	for _, host := range hosts {
		switch {
		case host == "@":
			grants.Apex = true
		case host == "" || strings.Contains(host, "."):
			// a scope host, which its scope's set answers
		case host == dnsEdgeSystemPublishHost:
			system = true
		default:
			grants.Wildcard = true
		}
	}
	if !grants.Wildcard && system && mailNetwork != nil && mailNetwork() {
		grants.Wildcard = true
	}
	return grants
}

// The scope a host names: <label>.<scope>, two labels, the second the scope, as §8.4's dns: hosts and §9's tls/
// hosts spell one; false for any other host.
func dnsEdgeScopeOfHost(host string) (string, bool) {
	label, scope, found := strings.Cut(host, ".")
	if !found || label == "" || scope == "" || strings.Contains(scope, ".") {
		return "", false
	}
	return scope, true
}

// Each of scopeLabels to whether hosts publish a host of it (the file header's grants row): <label>.<scope>, a glob
// label included, or a bare *, which takes every host. A host naming a scope scopeLabels does not hold sets nothing.
func DnsEdgeScopeWildcardsOfHosts(hosts []string, scopeLabels []string) map[string]bool {
	wildcards := map[string]bool{}
	for _, scope := range scopeLabels {
		wildcards[scope] = false
	}
	for _, host := range hosts {
		if host == "*" {
			for scope := range wildcards {
				wildcards[scope] = true
			}
			continue
		}
		if scope, ok := dnsEdgeScopeOfHost(host); ok {
			if _, listed := wildcards[scope]; listed {
				wildcards[scope] = true
			}
		}
	}
	return wildcards
}

// What a network's verified state publishes, as the edge records read it.
type DnsEdgePublished struct {
	// The names its rules publish (sidecar.AclState.PublishedNames), each with the expiry the gates hold it to.
	Names []*sidecar.AclPublishedName
	// Its Team scope labels (sidecar.ConfigState.TeamScopeLabels).
	ScopeLabels []string
}

// What a verified state publishes at now: acl's names and config's scope labels, a nil one giving none.
func DnsEdgePublishedOf(acl *sidecar.AclState, config *sidecar.ConfigState, now time.Time) *DnsEdgePublished {
	published := &DnsEdgePublished{}
	if acl != nil {
		published.Names = acl.PublishedNames(now)
	}
	if config != nil {
		published.ScopeLabels = config.TeamScopeLabels()
	}
	return published
}

// The hosts published at now, sorted and once each: a name past its expiry is dropped whatever the state answered,
// as §21.6's list drops it.
func (self *DnsEdgePublished) hostsAt(now time.Time) []string {
	hosts := []string{}
	if self == nil {
		return hosts
	}
	nowMs := now.UnixMilli()
	for _, name := range self.Names {
		if name == nil || name.ExpiresMs != 0 && name.ExpiresMs <= nowMs {
			continue
		}
		hosts = append(hosts, name.Host)
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// What one sync writes in a network's zone: the zone's own grants and each Team scope's *.
type DnsEdgeZoneGrants struct {
	Network DnsEdgeGrants
	// Each Team scope label the verified state lists, to whether a host of it is published.
	ScopeWildcards map[string]bool
}

// The grants of what a state publishes at now (the file header's grants row). mailNetwork answers whether the
// network has a mail_network row, asked only where the system publish alone would set *; nil answers no.
func DnsEdgeZoneGrantsOf(published *DnsEdgePublished, now time.Time, mailNetwork func() bool) *DnsEdgeZoneGrants {
	hosts := published.hostsAt(now)
	scopeLabels := []string{}
	if published != nil {
		scopeLabels = published.ScopeLabels
	}
	return &DnsEdgeZoneGrants{
		Network:        DnsEdgeGrantsOfHosts(hosts, mailNetwork),
		ScopeWildcards: DnsEdgeScopeWildcardsOfHosts(hosts, scopeLabels),
	}
}

// Whether a zone's state, nil for none, already answers grants: a zone whose flags are them, or no zone and no grant.
func dnsEdgeGrantsHeld(state *DnsZoneState, grants DnsEdgeGrants) bool {
	if state == nil {
		return grants == DnsEdgeGrants{}
	}
	return state.EdgeWildcard == grants.Wildcard && state.EdgeApex == grants.Apex
}

// Writes grants as the network's zone's edge flags in root, its own and none of its scopes', the zone and its label
// drawn on a first publish. Returns the write, nil when nothing was written. A restoring root or a suspended zone
// answers as DnsSyncEdgeRecords does (IsDnsZoneWaiting), with nothing written.
func SyncDnsEdgeRecords(ctx context.Context, networkId server.Id, root string, grants DnsEdgeGrants) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || root == "" {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	var state *DnsZoneState
	server.Db(ctx, func(conn server.PgConn) {
		state = getDnsZoneStateInTx(ctx, conn, root, networkId)
	})
	if dnsEdgeGrantsHeld(state, grants) {
		return nil, nil
	}
	return DnsSyncEdgeRecords(ctx, networkId, root, grants.Wildcard, grants.Apex)
}

// What one sync of a network's edge records did. Counts only: no network, scope or name.
type DnsEdgeSyncResult struct {
	// journal writes, each one zone's flags or one scope's set that changed
	WriteCount int
	// scope sets a host suspension holds, left for a later sync (§12.9)
	WaitingScopeCount int
	// Team scopes whose first set §12.5's rules refused (dnsservice.ErrDnsScopeLabelRefused), left unwritten
	RefusedScopeCount int
}

// Writes grants in the network's zone in root (the file header's sync row): the zone's flags first, its zone and
// label drawn on a first publish, then each scope set that differs. A restoring root or a suspended zone answers
// IsDnsZoneWaiting's errors, with that write and every later one not made; a scope a host suspension holds, or
// whose first set a rule of §12.5 refuses, is counted and skipped, and another scope's failure is joined into the
// error once every scope was tried.
func SyncDnsEdgeZones(ctx context.Context, networkId server.Id, root string, grants *DnsEdgeZoneGrants) (*DnsEdgeSyncResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || root == "" {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	var zone *DnsZoneState
	server.Db(ctx, func(conn server.PgConn) {
		zone = getDnsZoneStateInTx(ctx, conn, root, networkId)
	})
	return syncDnsEdgeZones(ctx, networkId, root, zone, grants)
}

// SyncDnsEdgeZones in root, canonical, from zone, the zone's state as the caller read it, nil for none.
func syncDnsEdgeZones(ctx context.Context, networkId server.Id, root string, zone *DnsZoneState, grants *DnsEdgeZoneGrants) (*DnsEdgeSyncResult, error) {
	result := &DnsEdgeSyncResult{}
	scopePublished := false
	for _, published := range grants.ScopeWildcards {
		scopePublished = scopePublished || published
	}
	switch {
	case zone == nil && grants.Network == (DnsEdgeGrants{}) && !scopePublished:
		// no zone and no grant: only a first publish draws a label (§12.5)
		return result, nil
	case zone == nil || !dnsEdgeGrantsHeld(zone, grants.Network):
		// the zone's flags, the zone and its label drawn on a first publish, a scope host's included
		written, err := DnsSyncEdgeRecords(ctx, networkId, root, grants.Network.Wildcard, grants.Network.Apex)
		if err != nil {
			return result, err
		}
		if written != nil {
			result.WriteCount += 1
		}
	}
	if len(grants.ScopeWildcards) == 0 {
		return result, nil
	}

	// each scope's set against the zone's rows, read with no lock: a write reads them again under the zone's lock
	label := ""
	var rows []*dnsStoredRow
	zone = nil
	server.Db(ctx, func(conn server.PgConn) {
		zone = getDnsZoneStateInTx(ctx, conn, root, networkId)
		label = getNetworkLabelInTx(ctx, conn, networkId, root)
		if zone != nil && label != "" {
			rows = getDnsZoneRowsInTx(ctx, conn, root, networkId)
		}
	})
	if zone == nil || label == "" {
		return result, nil
	}
	apex := label + "." + root
	scopes := make([]string, 0, len(grants.ScopeWildcards))
	for scope := range grants.ScopeWildcards {
		scopes = append(scopes, scope)
	}
	slices.Sort(scopes)
	errs := []error{}
	for _, scope := range scopes {
		wildcard := grants.ScopeWildcards[scope]
		if dnsScopeWritten(dnsservice.DnsScopeApex(apex, scope), rows) {
			state := &DnsScopeState{Root: root, Apex: apex, Scope: scope}
			dnsScopeStateFromRows(state, rows)
			if state.EdgeWildcard == wildcard && !state.EdgeApex && state.Mail == zone.Mail {
				continue
			}
		}
		edgeApex := false
		written, err := dnsSetScopeRecords(ctx, &DnsScopeArgs{
			Root:         root,
			NetworkId:    networkId,
			Scope:        scope,
			EdgeWildcard: &wildcard,
			EdgeApex:     &edgeApex,
			MailOfZone:   true,
		}, scopes)
		switch {
		case err == nil:
			if written != nil {
				result.WriteCount += 1
			}
		case errors.Is(err, dnsservice.ErrDnsScopeLabelRefused):
			result.RefusedScopeCount += 1
		case errors.Is(err, ErrDnsHostSuspended):
			result.WaitingScopeCount += 1
		case IsDnsZoneWaiting(err):
			// every later write to the zone is refused alike
			return result, err
		default:
			errs = append(errs, err)
		}
	}
	return result, errors.Join(errs...)
}

// The network's verified state as this instance's verifier holds it, nil before base 1: at or past every entry this
// instance landed, and read with no store request once the network is loaded, as Admitted reads it.
func (self *ConfigLogSequencer) heldState(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return nil, err
	}
	if network.log == nil {
		return nil, nil
	}
	return network.log.State(), nil
}

// What a verified state publishes at now; nil, before base 1, publishes nothing.
func dnsEdgePublishedOfState(state *sidecar.ConfigState, now time.Time) (*DnsEdgePublished, error) {
	if state == nil {
		return &DnsEdgePublished{}, nil
	}
	acl, err := state.Acl()
	if err != nil {
		return nil, err
	}
	return DnsEdgePublishedOf(acl, state, now), nil
}

// api's sync after an accepted entry (the file header's api row): the network's state as sequencer holds it, at or
// past the entry, its grants at the sequencer's now, written in the root api serves (DnsUserRoot). Best effort,
// since the entry has committed whatever happens here: a failure is logged and the hourly pass mends it. Does nothing
// in a binary that links no zones, a process that serves no root, or with no sequencer.
func SyncDnsEdgeRecordsAtTip(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id) {
	syncDnsEdgeRecordsAtTip(ctx, sequencer, networkId, DnsUserRoot())
}

// SyncDnsEdgeRecordsAtTip in root.
func syncDnsEdgeRecordsAtTip(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id, root string) {
	if sequencer == nil || root == "" || !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return
	}
	if r := server.HandleError(func() {
		state, err := sequencer.heldState(ctx, networkId)
		if err != nil {
			glog.Warningf("[dns]%s edge records wait for the pass, its state unread: %v\n", networkId, err)
			return
		}
		now := sequencer.now().UTC()
		published, err := dnsEdgePublishedOfState(state, now)
		if err != nil {
			glog.Warningf("[dns]%s edge records wait for the pass, its grants unread: %v\n", networkId, err)
			return
		}
		grants := DnsEdgeZoneGrantsOf(published, now, func() bool {
			return GetMailNetwork(ctx, networkId) != nil
		})
		synced, err := SyncDnsEdgeZones(ctx, networkId, root, grants)
		switch {
		case err == nil:
		case IsDnsZoneWaiting(err):
			if glog.V(1) {
				glog.Infof("[dns]%s edge records wait for the pass: %v\n", networkId, err)
			}
		default:
			glog.Warningf("[dns]%s edge records wait for the pass: %v\n", networkId, err)
		}
		// a count, never a scope's label (I-13)
		if synced != nil && 0 < synced.RefusedScopeCount {
			glog.Warningf("[dns]%s: §12.5's rules refuse %d team scopes' first sets\n", networkId, synced.RefusedScopeCount)
		}
	}); r != nil {
		glog.Warningf("[dns]%s edge records wait for the pass: %v\n", networkId, r)
	}
}

// Where the hourly pass reads each network's DnsEdgePublished: its newest verified state, which taskworker reads
// from its gate cache. A network with no grant answers an empty one; a state the source cannot read answers an
// error, never an empty one, so the pass does not take unread grants for none.
type DnsEdgePublishedSource func(ctx context.Context, networkId server.Id, now time.Time) (*DnsEdgePublished, error)

var dnsEdgePublishedSource atomic.Pointer[DnsEdgePublishedSource]

// Installs where this process's pass reads each network's published state, and answers what removes this install
// unless a newer one has replaced it. nil removes whatever is installed.
func SetDnsEdgePublishedSource(source DnsEdgePublishedSource) (remove func()) {
	if source == nil {
		dnsEdgePublishedSource.Store(nil)
		return func() {}
	}
	installed := &source
	dnsEdgePublishedSource.Store(installed)
	return func() {
		dnsEdgePublishedSource.CompareAndSwap(installed, nil)
	}
}

// Tunables of the hourly pass.
type DnsEdgeRecordsReconcileSettings struct {
	// Networks read from network_acl_head, and their zones' flags, at a time. Non-positive reads one at a time.
	NetworkBatchCount int
	// The most networks one run reads; past it the run answers where it stopped and the next run resumes there.
	// Non-positive reads one a run, so a run always moves the walk on.
	RunNetworkMaxCount int
}

func DefaultDnsEdgeRecordsReconcileSettings() *DnsEdgeRecordsReconcileSettings {
	return &DnsEdgeRecordsReconcileSettings{
		NetworkBatchCount:  200,
		RunNetworkMaxCount: 20000,
	}
}

// What one run of the pass did. Counts only: no network, scope or name.
type DnsEdgeRecordsReconcileResult struct {
	// networks whose grants were read
	NetworkCount int `json:"network_count"`
	// journal writes, each one zone's flags or one scope's set
	WriteCount int `json:"write_count"`
	// zones a restoring root or a suspension held, and scope sets a host suspension held, left for the next pass
	WaitingCount int `json:"waiting_count"`
	// zones whose write failed, logged and left for the next pass
	FailedCount int `json:"failed_count"`
	// Team scopes whose first set §12.5's rules refused, left unwritten
	RefusedScopeCount int `json:"refused_scope_count"`
	// where the next run resumes, nil once every network was read
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

// The edge flags and mail of each listed network's zone in root, by network; a network with no zone is absent.
func getDnsZoneEdgeStates(ctx context.Context, root string, networkIds []server.Id) map[server.Id]*DnsZoneState {
	states := map[server.Id]*DnsZoneState{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT network_id, edge_wildcard, edge_apex, mail FROM network_dns_zone WHERE root = $1 AND network_id = ANY($2)`,
			root,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				state := &DnsZoneState{}
				server.Raise(result.Scan(&networkId, &state.EdgeWildcard, &state.EdgeApex, &state.Mail))
				states[networkId] = state
			}
		})
	})
	return states
}

// One run of the hourly pass at now (the file header's pass row), from the network after afterNetworkId, nil for
// the first, in DnsUserRoot. A process with no published-state source answers ErrDnsEdgePublishedUnavailable and
// writes nothing, as does a source that fails for a network: a pass that cannot read the grants must not take them
// for none, so the run stops there and is tried again. A zone's own write that fails is logged and counted, and the
// run goes on. A binary that links no zones, or a process that serves no root, reads nothing.
func ReconcileDnsEdgeRecords(
	ctx context.Context,
	now time.Time,
	afterNetworkId *server.Id,
	settings *DnsEdgeRecordsReconcileSettings,
) (*DnsEdgeRecordsReconcileResult, error) {
	result := &DnsEdgeRecordsReconcileResult{}
	root := DnsUserRoot()
	if root == "" || !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return result, nil
	}
	sourcePointer := dnsEdgePublishedSource.Load()
	if sourcePointer == nil {
		return nil, ErrDnsEdgePublishedUnavailable
	}
	source := *sourcePointer
	root = dnsLabelRoot(root)
	after := server.Id{}
	if afterNetworkId != nil {
		after = *afterNetworkId
	}
	batchCount := max(settings.NetworkBatchCount, 1)
	runMaxCount := max(settings.RunNetworkMaxCount, 1)
	for {
		limit := min(batchCount, runMaxCount-result.NetworkCount)
		if limit <= 0 {
			stopped := after
			result.AfterNetworkId = &stopped
			return result, nil
		}
		networkIds := tlsPublishedNamesNetworks(ctx, after, false, limit)
		if len(networkIds) == 0 {
			return result, nil
		}
		zones := getDnsZoneEdgeStates(ctx, root, networkIds)
		for _, networkId := range networkIds {
			published, err := source(ctx, networkId, now)
			if err != nil {
				return nil, err
			}
			grants := DnsEdgeZoneGrantsOf(published, now, func() bool {
				return GetMailNetwork(ctx, networkId) != nil
			})
			result.NetworkCount += 1
			after = networkId
			var synced *DnsEdgeSyncResult
			var syncErr error
			if r := server.HandleError(func() {
				synced, syncErr = syncDnsEdgeZones(ctx, networkId, root, zones[networkId], grants)
			}); r != nil {
				syncErr = fmt.Errorf("the zone's write failed: %v", r)
			}
			if synced != nil {
				result.WriteCount += synced.WriteCount
				result.WaitingCount += synced.WaitingScopeCount
				result.RefusedScopeCount += synced.RefusedScopeCount
			}
			switch {
			case syncErr == nil:
			case IsDnsZoneWaiting(syncErr):
				result.WaitingCount += 1
			default:
				result.FailedCount += 1
				glog.Warningf("[dns]%s edge records wait for the next pass: %v\n", networkId, syncErr)
			}
		}
	}
}
