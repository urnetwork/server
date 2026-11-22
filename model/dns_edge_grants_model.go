package model

// §12.7's edge address records follow the network's edge grants (owner decision 335; plan-DNS-edge-record-trigger,
// QD-130): * answers the healthy lb interfaces while the network's verified state publishes any host of its zone but
// @, or the system publish, and @ while it publishes @. DnsSyncEdgeRecordsInTx (dns_zone_model.go) writes the two
// flags and their rows; this file decides the flags and calls it, which DNS-M5-11 left to the callers.
//
//	grants    a state's published names (sidecar.AclState.PublishedNames at now, §21.6's list, by the agreement
//	          rule PublishGrant reads): a one-label host or a glob sets *, @ sets @, and the system publish's host,
//	          mta-sts, sets * only while the network has a mail_network row, since mta-sts.<net>.<root> has no row
//	          of its own and resolves only through * (§13.9). A scope host, <host>.<scope>, is its Team scope's
//	          set's (DnsScopeState) and sets neither here
//	api       after each accepted POST /network/config/delta or /network/config/projection, once its entry's PUT
//	          has committed, api reads the network's state at the store's tip and syncs (SyncDnsEdgeRecordsAtTip),
//	          best effort: the entry stands whatever the sync does, a failure is logged, and the pass mends it
//	pass      every hour taskworker walks the networks with a log in id order, each one's names from the
//	          process's published-names source (QD-770), and syncs each (ReconcileDnsEdgeRecords): a grant that
//	          expired, which no entry marks, loses its records, and a sync a crash lost between an entry's PUT and
//	          its write is made
//	sync      a zone whose flags already hold the grants is not written, and no zone is made for a network with no
//	          grant: only a first publish draws a label (§12.5). Otherwise one DnsSyncEdgeRecords, one journal row a
//	          transition, in a transaction of its own, since §7.9 commits by a conditional PUT and has no
//	          transaction to join
//
// Two syncs of one network may race, each from the state it read, so the later commit may carry the older state;
// the next entry's sync or the hourly pass brings it back, the lag §12.7 accepts. Whether a listener is up never
// shows: §9.4's FirstListen and LastUnlisten have no DNS consumer.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The system publish's host (§13.9): tls/443/mta-sts's.
const dnsEdgeSystemPublishHost = "mta-sts"

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

// The grants of the network's verified state at now; none before base 1.
func dnsEdgeGrantsOfState(ctx context.Context, networkId server.Id, state *sidecar.ConfigState, now time.Time) (DnsEdgeGrants, error) {
	if state == nil {
		return DnsEdgeGrants{}, nil
	}
	acl, err := state.Acl()
	if err != nil {
		return DnsEdgeGrants{}, err
	}
	hosts := []string{}
	for _, name := range acl.PublishedNames(now) {
		hosts = append(hosts, name.Host)
	}
	return DnsEdgeGrantsOfHosts(hosts, func() bool {
		return GetMailNetwork(ctx, networkId) != nil
	}), nil
}

// Whether a zone's state, nil for none, already answers grants: a zone whose flags are them, or no zone and no grant.
func dnsEdgeGrantsHeld(state *DnsZoneState, grants DnsEdgeGrants) bool {
	if state == nil {
		return grants == DnsEdgeGrants{}
	}
	return state.EdgeWildcard == grants.Wildcard && state.EdgeApex == grants.Apex
}

// Writes grants as the network's zone's edge flags in root (the file header's sync row), the zone and its label
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

// api's sync after an accepted entry (the file header's api row): the network's state at the store's tip through
// sequencer, its grants at now, written in the root api serves (DnsUserRoot). Best effort, since the entry has
// committed whatever happens here: a failure is logged and the hourly pass mends it. Does nothing in a binary that
// links no zones or a process that serves no root.
func SyncDnsEdgeRecordsAtTip(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id) {
	root := DnsUserRoot()
	if sequencer == nil || root == "" || !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return
	}
	if r := server.HandleError(func() {
		state, err := sequencer.CaughtUpState(ctx, networkId)
		if err != nil {
			glog.Warningf("[dns]%s edge records wait for the pass, its state unread: %v\n", networkId, err)
			return
		}
		grants, err := dnsEdgeGrantsOfState(ctx, networkId, state, server.NowUtc())
		if err != nil {
			glog.Warningf("[dns]%s edge records wait for the pass, its grants unread: %v\n", networkId, err)
			return
		}
		_, err = SyncDnsEdgeRecords(ctx, networkId, root, grants)
		switch {
		case err == nil:
		case IsDnsZoneWaiting(err):
			if glog.V(1) {
				glog.Infof("[dns]%s edge records wait for the pass: %v\n", networkId, err)
			}
		default:
			glog.Warningf("[dns]%s edge records wait for the pass: %v\n", networkId, err)
		}
	}); r != nil {
		glog.Warningf("[dns]%s edge records wait for the pass: %v\n", networkId, r)
	}
}

// Tunables of the hourly pass.
type DnsEdgeRecordsReconcileSettings struct {
	// Networks read from network_acl_head, and their zones' flags, at a time.
	NetworkBatchCount int
	// The most networks one run reads; past it the run answers where it stopped and the next run resumes there.
	RunNetworkMaxCount int
}

func DefaultDnsEdgeRecordsReconcileSettings() *DnsEdgeRecordsReconcileSettings {
	return &DnsEdgeRecordsReconcileSettings{
		NetworkBatchCount:  200,
		RunNetworkMaxCount: 20000,
	}
}

// What one run of the pass did. Counts only: no network or name.
type DnsEdgeRecordsReconcileResult struct {
	// networks whose grants were read
	NetworkCount int `json:"network_count"`
	// zones written, each one transition
	WriteCount int `json:"write_count"`
	// zones a restoring root or a suspension held, left for the next pass
	WaitingCount int `json:"waiting_count"`
	// zones whose write failed, logged and left for the next pass
	FailedCount int `json:"failed_count"`
	// where the next run resumes, nil once every network was read
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

// The edge flags of each listed network's zone in root, by network; a network with no zone is absent.
func getDnsZoneEdgeStates(ctx context.Context, root string, networkIds []server.Id) map[server.Id]*DnsZoneState {
	states := map[server.Id]*DnsZoneState{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT network_id, edge_wildcard, edge_apex FROM network_dns_zone WHERE root = $1 AND network_id = ANY($2)`,
			root,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				state := &DnsZoneState{}
				server.Raise(result.Scan(&networkId, &state.EdgeWildcard, &state.EdgeApex))
				states[networkId] = state
			}
		})
	})
	return states
}

// One run of the hourly pass at now (the file header's pass row), from the network after afterNetworkId, nil for
// the first, in DnsUserRoot. A process with no published-names source answers ErrTlsPublishedNamesUnavailable and
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
	sourcePointer := tlsPublishedNamesSource.Load()
	if sourcePointer == nil {
		return nil, ErrTlsPublishedNamesUnavailable
	}
	source := *sourcePointer
	root = dnsLabelRoot(root)
	after := server.Id{}
	if afterNetworkId != nil {
		after = *afterNetworkId
	}
	for {
		limit := min(settings.NetworkBatchCount, settings.RunNetworkMaxCount-result.NetworkCount)
		if limit <= 0 {
			stopped := after
			result.AfterNetworkId = &stopped
			return result, nil
		}
		networkIds := tlsPublishedNamesNetworks(ctx, after, false, limit)
		if len(networkIds) == 0 {
			return result, nil
		}
		states := getDnsZoneEdgeStates(ctx, root, networkIds)
		for _, networkId := range networkIds {
			names, err := source(ctx, networkId, now)
			if err != nil {
				return nil, err
			}
			hosts := []string{}
			for _, name := range names {
				// what has expired at now is dropped whatever the source answered, as §21.6's list drops it
				if name == nil || !name.ExpireTime.IsZero() && !now.Before(name.ExpireTime) {
					continue
				}
				hosts = append(hosts, name.Host)
			}
			grants := DnsEdgeGrantsOfHosts(hosts, func() bool {
				return GetMailNetwork(ctx, networkId) != nil
			})
			result.NetworkCount += 1
			after = networkId
			if dnsEdgeGrantsHeld(states[networkId], grants) {
				continue
			}
			var written *DnsWriteResult
			var writeErr error
			if r := server.HandleError(func() {
				written, writeErr = DnsSyncEdgeRecords(ctx, networkId, root, grants.Wildcard, grants.Apex)
			}); r != nil {
				writeErr = fmt.Errorf("the zone's write failed: %v", r)
			}
			switch {
			case writeErr == nil:
				if written != nil {
					result.WriteCount += 1
				}
			case IsDnsZoneWaiting(writeErr):
				result.WaitingCount += 1
			default:
				result.FailedCount += 1
				glog.Warningf("[dns]%s edge records wait for the next pass: %v\n", networkId, writeErr)
			}
		}
	}
}
