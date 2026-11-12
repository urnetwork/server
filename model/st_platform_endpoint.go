// The SN25 extender amendment's platform endpoints (sn WHITEPAPER.md §8.6.2):
// client identities the network operator itself runs to serve a platform
// feature, registered by the operator. A contract whose hop is a platform
// endpoint is intra-platform, and that hop's share joins the extender pool's Z
// instead of the provider rows. The class is fixed when the contract is created,
// never inferred from a missing participant: a share is a platform endpoint's
// exactly when the contract was created while the endpoint's registration was in
// force.
//
// The registry reads §8.6 narrowly (the owner's decisions of 2026-11-12):
//   - The kinds are the signed set. At E_x the platform endpoints are sidecar's
//     edge platform clients, its TLS, SMTP and MX edges: tlsedge, smtpedge and
//     mxedge (decision 6). Every other kind is refused before anything is read.
//     A new kind is a new rule: it needs a new signed amendment entry before it
//     is added here.
//   - "On an operator network" is the test. A registration names an active
//     client on one of the operator's own networks (its gated durable networks
//     or the networks it declares it controls, the networks E5 names), never on
//     a declared measurement network.
//   - A registration holds from the moment it is made, never backdated.
//
// Sidecar has not shipped, so the registry starts empty and intra-platform bytes
// are 0 until the operator registers an endpoint. Registrations are kept
// forever: a revocation only ends one, so an epoch read again classifies its
// contracts as it did.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

// The kinds of platform endpoint: sidecar's edge platform clients, the set the
// signed amendment entry covers. Adding one needs a new signed entry.
const (
	StPlatformEndpointTlsEdge  = "tlsedge"
	StPlatformEndpointSmtpEdge = "smtpedge"
	StPlatformEndpointMxEdge   = "mxedge"
)

// StPlatformEndpointKinds are the kinds a registration may name.
var StPlatformEndpointKinds = []string{StPlatformEndpointTlsEdge, StPlatformEndpointSmtpEdge, StPlatformEndpointMxEdge}

// StPlatformEndpoint is one registration of a client as a platform endpoint.
type StPlatformEndpoint struct {
	ClientId     server.Id
	NetworkId    server.Id
	Kind         string
	RegisterTime time.Time
	RegisteredBy string
	// RevokeTime ends the registration; nil while it is in force.
	RevokeTime *time.Time
	RevokedBy  string
}

// Holds reports whether the registration was in force at t: from its
// registration, inclusive, until its revocation, exclusive.
func (self *StPlatformEndpoint) Holds(t time.Time) bool {
	return !t.Before(self.RegisterTime) && (self.RevokeTime == nil || t.Before(*self.RevokeTime))
}

// StPlatformEndpointSet is every registration, by client.
type StPlatformEndpointSet map[server.Id][]*StPlatformEndpoint

// Holds reports whether clientId was a registered platform endpoint when a
// contract was created at createTime.
func (self StPlatformEndpointSet) Holds(clientId server.Id, createTime time.Time) bool {
	for _, registration := range self[clientId] {
		if registration.Holds(createTime) {
			return true
		}
	}
	return false
}

// Count is the number of registrations, revoked or not.
func (self StPlatformEndpointSet) Count() int {
	count := 0
	for _, registrations := range self {
		count += len(registrations)
	}
	return count
}

// InForce is the number of clients registered at t.
func (self StPlatformEndpointSet) InForce(t time.Time) int {
	count := 0
	for clientId := range self {
		if self.Holds(clientId, t) {
			count++
		}
	}
	return count
}

// ErrStPlatformEndpoint refuses a registration or revocation.
var ErrStPlatformEndpoint = errors.New("platform endpoint refused")

// StPlatformEndpointNetworks are the operator's networks a registration is
// checked against: its own networks, which a platform endpoint must be on, and
// its declared measurement networks, which it must not be on.
type StPlatformEndpointNetworks struct {
	Operator    map[server.Id]bool
	Measurement map[server.Id]bool
}

// RegisterStPlatformEndpoint registers clientId as a platform endpoint of kind
// from now on. It is refused, before anything is read, for a kind other than
// sidecar's three edges, the signed set; and for a client that does not exist or
// is not active, a client whose network is not one of the operator's own or is a
// measurement network, a client already registered, or an empty by.
func RegisterStPlatformEndpoint(ctx context.Context, clientId server.Id, kind string, by string, networks StPlatformEndpointNetworks) (*StPlatformEndpoint, error) {
	if !slices.Contains(StPlatformEndpointKinds, kind) {
		return nil, fmt.Errorf("%w: kind %q is not one of %v, the kinds the signed amendment entry covers; a new kind needs a new signed entry", ErrStPlatformEndpoint, kind, StPlatformEndpointKinds)
	}
	if by == "" {
		return nil, fmt.Errorf("%w: a registration names who makes it", ErrStPlatformEndpoint)
	}
	var registered *StPlatformEndpoint
	var refused error
	server.Tx(ctx, func(tx server.PgTx) {
		var networkId server.Id
		var active bool
		result, err := tx.Query(ctx, `SELECT network_id, active FROM network_client WHERE client_id = $1`, clientId)
		found := false
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId, &active))
				found = true
			}
		})
		switch {
		case !found || !active:
			refused = fmt.Errorf("%w: client %s is not an active client", ErrStPlatformEndpoint, clientId)
			return
		case !networks.Operator[networkId]:
			refused = fmt.Errorf("%w: client %s is on network %s, which is not one of the operator's own networks (durable_network_ids or extender_pool.declared_network_ids)", ErrStPlatformEndpoint, clientId, networkId)
			return
		case networks.Measurement[networkId]:
			refused = fmt.Errorf("%w: client %s is on a declared measurement network, %s", ErrStPlatformEndpoint, clientId, networkId)
			return
		}
		now := server.NowUtc()
		tag := server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_platform_endpoint (client_id, network_id, kind, register_time, registered_by)
			SELECT $1, $2, $3, $4, $5
			WHERE NOT EXISTS (SELECT 1 FROM st_platform_endpoint WHERE client_id = $1 AND revoke_time IS NULL)
		`, clientId, networkId, kind, now, by))
		if tag.RowsAffected() == 0 {
			refused = fmt.Errorf("%w: client %s is already registered", ErrStPlatformEndpoint, clientId)
			return
		}
		registered = &StPlatformEndpoint{ClientId: clientId, NetworkId: networkId, Kind: kind, RegisterTime: now, RegisteredBy: by}
	})
	return registered, refused
}

// RevokeStPlatformEndpoint ends clientId's registration now. Contracts created
// before keep their class.
func RevokeStPlatformEndpoint(ctx context.Context, clientId server.Id, by string) error {
	if by == "" {
		return fmt.Errorf("%w: a revocation names who makes it", ErrStPlatformEndpoint)
	}
	var refused error
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(ctx, `
			UPDATE st_platform_endpoint SET revoke_time = $2, revoked_by = $3
			WHERE client_id = $1 AND revoke_time IS NULL AND register_time < $2
		`, clientId, server.NowUtc(), by))
		if tag.RowsAffected() == 0 {
			refused = fmt.Errorf("%w: client %s has no registration in force to revoke", ErrStPlatformEndpoint, clientId)
		}
	})
	return refused
}

// GetStPlatformEndpoints is every registration, revoked or not, by client, each
// client's in registration order.
func GetStPlatformEndpoints(ctx context.Context) StPlatformEndpointSet {
	set := StPlatformEndpointSet{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT client_id, network_id, kind, register_time, registered_by, revoke_time, revoked_by
			FROM st_platform_endpoint
			ORDER BY client_id, register_time
		`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				registration := &StPlatformEndpoint{}
				var revokedBy *string
				server.Raise(result.Scan(&registration.ClientId, &registration.NetworkId, &registration.Kind, &registration.RegisterTime,
					&registration.RegisteredBy, &registration.RevokeTime, &revokedBy))
				if revokedBy != nil {
					registration.RevokedBy = *revokedBy
				}
				set[registration.ClientId] = append(set[registration.ClientId], registration)
			}
		})
	})
	return set
}
