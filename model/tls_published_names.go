package model

// §21.6's list of what a network publishes (TLS-M5-13; I-18): every active
// publish grant, host, port, label or alias, audience and expiry, paged for
// the hourly check that looks each name up against the blocklists.
//
// The grants are the projection's, not a table's: nothing in Postgres lists
// them, so this pages the networks that have a log (network_acl_head, MG2)
// and asks this process's installed source for each one's grants, as a gate
// asks for a network's verified state (SetApprovalGateStates). A process with
// no source answers ErrTlsPublishedNamesUnavailable and never an empty list,
// so a check that cannot read the grants does not pass for one that found
// none. A source that fails for one network fails the page, for the same
// reason: the check retries rather than skip a network.
//
// A grant past its expiry at now is dropped here whatever the source
// answered. A page ends at Limit names and its cursor is the last name it
// gave, so a network's names may span pages and none is given twice.

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
)

// This process cannot list published names: it does not link MG2, or no
// source is installed.
var ErrTlsPublishedNamesUnavailable = errors.New("tls published names are unavailable in this process")

// One active publish grant of a network, tls/<port>/<host>.
type TlsPublishedName struct {
	NetworkId server.Id
	// The grant's host, one label as the network's document names it.
	Host string
	Port int
	// The name it is served under: Host under the network's edge label, or
	// under an alias when Alias is set.
	Label string
	Alias string
	// members, members-device, link or public.
	Audience protocol.AclAudience
	// When the last of its rules ends; zero for none.
	ExpireTime time.Time
}

// Where a page resumes: after this name, in (network, host, port) order.
type TlsPublishedNamesCursor struct {
	NetworkId server.Id `json:"network_id"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
}

// A network's active publish grants at now, from its newest verified
// projection. A network with none answers an empty list and no error.
type TlsPublishedNamesSource func(ctx context.Context, networkId server.Id, now time.Time) ([]*TlsPublishedName, error)

var tlsPublishedNamesSource atomic.Pointer[TlsPublishedNamesSource]

// Installs where this process reads each network's publish grants; nil
// removes it, and every list answers unavailable.
func SetTlsPublishedNamesSource(source TlsPublishedNamesSource) {
	if source == nil {
		tlsPublishedNamesSource.Store(nil)
		return
	}
	tlsPublishedNamesSource.Store(&source)
}

// Tunables of the list.
type TlsPublishedNamesSettings struct {
	// The most names one page gives.
	Limit int
	// Networks read from network_acl_head at a time.
	NetworkBatchCount int
}

func DefaultTlsPublishedNamesSettings() *TlsPublishedNamesSettings {
	return &TlsPublishedNamesSettings{
		Limit:             1000,
		NetworkBatchCount: 200,
	}
}

func tlsPublishedNameCompare(a *TlsPublishedName, b *TlsPublishedName) int {
	return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Port, b.Port))
}

// The networks with a log from afterNetworkId on in id order, itself too
// when inclusive, at most limit.
func tlsPublishedNamesNetworks(ctx context.Context, afterNetworkId server.Id, inclusive bool, limit int) (networkIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id
				FROM network_acl_head
				WHERE $1 < network_id OR ($2 AND network_id = $1)
				ORDER BY network_id
				LIMIT $3
			`,
			afterNetworkId,
			inclusive,
			limit,
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

// One page of every network's active publish grants at now, after cursor, nil
// for the first page. The returned cursor resumes after the page's last name,
// and is nil once the list is done.
func ListTlsPublishedNames(ctx context.Context, cursor *TlsPublishedNamesCursor, now time.Time) ([]*TlsPublishedName, *TlsPublishedNamesCursor, error) {
	return ListTlsPublishedNamesWithSettings(ctx, cursor, now, DefaultTlsPublishedNamesSettings())
}

func ListTlsPublishedNamesWithSettings(
	ctx context.Context,
	cursor *TlsPublishedNamesCursor,
	now time.Time,
	settings *TlsPublishedNamesSettings,
) ([]*TlsPublishedName, *TlsPublishedNamesCursor, error) {
	sourcePointer := tlsPublishedNamesSource.Load()
	if !server.SidecarMg2Linked() || sourcePointer == nil {
		return nil, nil, ErrTlsPublishedNamesUnavailable
	}
	source := *sourcePointer
	names := []*TlsPublishedName{}
	afterNetworkId := server.Id{}
	inclusive := false
	if cursor != nil {
		afterNetworkId = cursor.NetworkId
		inclusive = true
	}
	for {
		networkIds := tlsPublishedNamesNetworks(ctx, afterNetworkId, inclusive, settings.NetworkBatchCount)
		if len(networkIds) == 0 {
			return names, nil, nil
		}
		for _, networkId := range networkIds {
			published, err := source(ctx, networkId, now)
			if err != nil {
				return nil, nil, err
			}
			active := []*TlsPublishedName{}
			for _, name := range published {
				if name == nil {
					continue
				}
				if !name.ExpireTime.IsZero() && !now.Before(name.ExpireTime) {
					continue
				}
				if cursor != nil && cursor.NetworkId == networkId {
					after := &TlsPublishedName{Host: cursor.Host, Port: cursor.Port}
					if tlsPublishedNameCompare(name, after) <= 0 {
						continue
					}
				}
				// the list's network, whatever the source wrote
				listed := *name
				listed.NetworkId = networkId
				active = append(active, &listed)
			}
			slices.SortFunc(active, tlsPublishedNameCompare)
			for _, name := range active {
				if settings.Limit <= len(names) {
					last := names[len(names)-1]
					return names, &TlsPublishedNamesCursor{NetworkId: last.NetworkId, Host: last.Host, Port: last.Port}, nil
				}
				names = append(names, name)
			}
		}
		afterNetworkId = networkIds[len(networkIds)-1]
		inclusive = false
	}
}
