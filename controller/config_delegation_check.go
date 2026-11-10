package controller

// api's half of §7.9's forwarding row (TEAM-M8-01; D-TEAM-33; owner decision
// 197): a forwarding Delegation's host client. The statement's form, its one
// signer and its sidecar's standing are the log's rule, which every reader
// judges alike (sidecar/config_forwarding.go). A reader holds no client rows,
// so api alone checks what the Delegation says of them, before it sequences
// it: host_client_id, where set, is the source client of the sidecar's current
// bound client (network_client.source_client_id), and host_client_public_key
// is that client's registered ClientKey. A Delegation naming no host client
// has nothing here to check, and one whose host client cannot be read is
// refused: a lookup that fails opens nothing.
//
// The host client is what lets a host verify the app device a forwarding key
// forwards for (§17.9), so a Delegation naming another client, or another key
// for the right one, would let a sidecar claim a device it does not run on.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The host client a statement names for forwarding.
type configForwardingHost struct {
	sidecarId           server.Id
	hostClientId        server.Id
	hostClientPublicKey []byte
}

// A host-client refusal, answered as the log's own refusals are.
func configDelegationRefused(text string) error {
	return &model.ConfigLogError{Status: http.StatusForbidden, Code: "config_refused", Err: errors.New(text)}
}

// The host client a Signed forwarding Delegation names, read from the bytes as
// received with no signature checked. nil for any other statement, a
// Delegation with no forward, and a forwarding Delegation naming no host
// client, each of which the log's rule judges whole. An error for a forward
// whose ids are malformed, which that rule refuses too.
func configDelegationForwardingHost(signedBytes []byte) (*configForwardingHost, error) {
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		return nil, nil
	}
	if bridge.SignedType(signed.GetType()) != bridge.SignedType_SIGNED_DELEGATION {
		return nil, nil
	}
	message, err := sidecar.ParseConfigStatement(bridge.SignedPurposeAuthority, bridge.SignedType_SIGNED_DELEGATION, signed.GetBody())
	if err != nil {
		return nil, nil
	}
	delegation, ok := message.(*protocol.Delegation)
	if !ok || !sidecar.ConfigDelegationForwards(delegation) {
		return nil, nil
	}
	forward := delegation.GetForward()
	if len(forward.GetHostClientId()) == 0 && len(forward.GetHostClientPublicKey()) == 0 {
		return nil, nil
	}
	sidecarId, err := server.IdFromBytes(forward.GetForwardSidecarId())
	if err != nil {
		return nil, fmt.Errorf("a forward_sidecar_id of %d bytes", len(forward.GetForwardSidecarId()))
	}
	hostClientId, err := server.IdFromBytes(forward.GetHostClientId())
	if err != nil {
		return nil, fmt.Errorf("a host_client_id of %d bytes", len(forward.GetHostClientId()))
	}
	if len(forward.GetHostClientPublicKey()) == 0 {
		return nil, errors.New("a host client with no key")
	}
	return &configForwardingHost{
		sidecarId:           sidecarId,
		hostClientId:        hostClientId,
		hostClientPublicKey: forward.GetHostClientPublicKey(),
	}, nil
}

// The source client of the sidecar's current bound client in networkId: the
// client its newest binding names, active, of an identity not revoked. found
// is false when the sidecar has no such client or that client is top-level.
// Needs MG1's tables: a binary without them finds none.
func configSidecarSourceClient(ctx context.Context, networkId server.Id, sidecarId server.Id) (server.Id, bool, error) {
	if !server.SidecarMg1Linked() {
		return server.Id{}, false, nil
	}
	var sourceClientId server.Id
	found := false
	recovered := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT network_client.source_client_id
					FROM sidecar_identity_binding
					INNER JOIN sidecar_identity ON sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id
					INNER JOIN network_client ON network_client.client_id = sidecar_identity_binding.client_id
					WHERE
						sidecar_identity_binding.sidecar_id = $1 AND
						sidecar_identity.network_id = $2 AND
						sidecar_identity.status = $3 AND
						network_client.network_id = $2 AND
						network_client.active AND
						network_client.source_client_id IS NOT NULL AND
						sidecar_identity_binding.generation = (
							SELECT MAX(newest.generation)
							FROM sidecar_identity_binding newest
							WHERE newest.sidecar_id = $1
						)
				`,
				sidecarId,
				networkId,
				model.SidecarIdentityStatusActive,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&sourceClientId))
					found = true
				}
			})
		})
	})
	if recovered != nil {
		// a failed read finds nothing and says so, so the caller answers 503 and never takes it as "no host client"
		return server.Id{}, false, fmt.Errorf("the sidecar's client could not be read: %v", recovered)
	}
	return sourceClientId, found, nil
}

// api's check of a statement before it is sequenced in networkId: nil for
// anything but a forwarding Delegation naming a host client; for one, nil only
// when the host client is the source client of its sidecar's current bound
// client and the key is that client's registered ClientKey. A refusal answers
// 403 config_refused, and a failed read 503.
func configDelegationHostCheck(ctx context.Context, networkId server.Id, signedBytes []byte) error {
	host, err := configDelegationForwardingHost(signedBytes)
	if err != nil {
		return configDelegationRefused(err.Error())
	}
	if host == nil {
		return nil
	}
	sourceClientId, found, err := configSidecarSourceClient(ctx, networkId, host.sidecarId)
	if err != nil {
		return &model.ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: err}
	}
	if !found || sourceClientId != host.hostClientId {
		return configDelegationRefused("the host client is not the source client of the sidecar's current client")
	}
	var registeredKey []byte
	if recovered := server.HandleError(func() {
		var keyErr error
		registeredKey, keyErr = model.GetClientPublicKey(ctx, host.hostClientId)
		server.Raise(keyErr)
	}); recovered != nil {
		return &model.ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: fmt.Errorf("the host client's key could not be read: %v", recovered)}
	}
	if len(registeredKey) == 0 || !bytes.Equal(registeredKey, host.hostClientPublicKey) {
		return configDelegationRefused("the host client key is not the host client's registered key")
	}
	return nil
}
