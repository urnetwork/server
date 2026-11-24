package controller

// §7.9's routes for the authorization log (TEAM-M2-13), Client class on the
// strict_exp allowlist (api/route_authz.go): POST /network/config/delta
// appends a statement, POST /network/config/projection a base, and GET
// /network/config/projection?after= reads entries, each verbatim, as JSON
// with base64 bytes (connect/api/bringyour.yml), so TestSpecConformance
// covers them and signed bytes never pass through a re-marshal.
//
// The bearer's own client's sidecar must stand admitted at the tip to append,
// save that an unadmitted client of a member may read the log and post a
// recovery-signed statement only (§7.9's Recovery `before admission` row),
// and base 1 comes from the network's first device, which carries its
// genesis: the owner's top-level client, bound to the sidecar the genesis
// names, and neither tagged nor ephemeral: before base 1 a tagged or
// ephemeral enrollment answers no_genesis (TEAM-M1-04), and base 1 refuses
// such a client whenever it was minted. The statements themselves are judged
// by their signatures and the log's rules (model/config_log_model.go), so
// this is defense in depth, not the authority.
//
// One check is api's alone: a forwarding Delegation's host client is the
// source client of its sidecar's current client, with that client's
// registered key (config_delegation_check.go; D-TEAM-33; owner decision 197),
// since no reader holds a client row.
//
// Once an append is answered, §12.7's edge address records follow the state
// it leaves, the zone's and each of its Team scopes' (model.SyncDnsEdgeRecordsAtTip;
// owner decision 335), best effort, so the answer never fails with the zone's
// write.
//
// A base that turns on an authorization feature some service serving the
// network does not implement is refused before anything is appended, 403
// feature_unsupported with the tip (DESIGN-COMPUTE §16.6's point row;
// CACL-M11-03): api reads only the base's rows' ProjectionSettings.auth_features,
// which carry its head's ServiceSettings, the newest base's, and the services'
// bits (server.SidecarConfig.AuthFeaturesFor), and judges no content. A bit the
// newest base already set turns nothing on, so only the bits a base adds are
// held to the services. api's verifier judges the base again at the tip as every
// reader does, a bit its own release lacks or rows clearing one included.
//
// Refusals answer {"error", "code", "tip"}: 403 config_refused, 409
// config_tip_moved, config_unadmitted's 403 and feature_unsupported's 403; an
// unavailable store or a process with no first metadata cluster answers 503.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// POST /network/config/delta's body.
type NetworkConfigDeltaArgs struct {
	// One Signed statement, verbatim.
	Signed []byte `json:"signed"`
}

// The entry's position and chain value, {n, A_n}.
type NetworkConfigPinResult struct {
	N     uint64 `json:"n"`
	Chain []byte `json:"chain"`
}

// POST /network/config/projection's body.
type NetworkConfigProjectionPublishArgs struct {
	// The Signed ProjectionHeader, verbatim.
	SignedHeader []byte `json:"signed_header"`
	// The ProjectionRows bytes, verbatim, at most 4 MiB.
	Rows []byte `json:"rows"`
	// The Signed Genesis, with base 1 only.
	Genesis []byte `json:"genesis,omitempty"`
	// The Signed EvsPush of each config version the base adds, oldest first.
	ConfigPushes [][]byte `json:"config_pushes,omitempty"`
}

// GET /network/config/projection's query.
type NetworkConfigProjectionArgs struct {
	// The newest position the caller holds; 0 reads from the genesis.
	After uint64
}

// GET /network/config/projection's answer.
type NetworkConfigProjectionResult struct {
	// The Signed Genesis, when the read starts at 0.
	Genesis []byte `json:"genesis,omitempty"`
	// The entries after the position asked, each AuthEntry verbatim.
	Entries [][]byte `json:"entries"`
	// The rows of each base among the entries, verbatim.
	Rows [][]byte `json:"rows"`
	// The newest entry read.
	Tip *NetworkConfigPinResult `json:"tip,omitempty"`
	// More entries follow the last one.
	More bool `json:"more"`
}

func configPinResult(pin *bridge.AuthPin) *NetworkConfigPinResult {
	if pin == nil {
		return nil
	}
	return &NetworkConfigPinResult{N: pin.GetVersion(), Chain: pin.GetChain()}
}

// An unadmitted client's refusal.
func configUnadmitted(text string) error {
	return &model.ConfigLogError{Status: http.StatusForbidden, Code: "config_unadmitted", Err: errors.New(text)}
}

// The code of a base turning on a feature a service serving the network lacks.
const configFeatureUnsupported = "feature_unsupported"

// Nil where the base's rows turn on no feature beyond those every service
// serving the network implements; else feature_unsupported, 403 with the tip.
// Rows that do not parse are the verifier's to refuse, as config_refused.
func configBaseFeatureCheck(clientSession *session.ClientSession, sequencer *model.ConfigLogSequencer, rows []byte) error {
	features, err := sidecar.ConfigRowsAuthFeatures(rows)
	if err != nil || features == 0 {
		return nil
	}
	networkId := clientSession.ByJwt.NetworkId
	state, err := sequencer.CaughtUpState(clientSession.Ctx, networkId)
	if err != nil {
		return &model.ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: err}
	}
	var on sidecar.AuthFeature
	var tip *bridge.AuthPin
	if state != nil {
		on, tip = state.AuthFeatures(), state.Pin()
	}
	if lacking := (features &^ on) &^ server.CurrentSidecarConfig().AuthFeaturesFor(networkId); lacking != 0 {
		return &model.ConfigLogError{
			Status: http.StatusForbidden,
			Code:   configFeatureUnsupported,
			Tip:    tip,
			Err:    fmt.Errorf("a service serving the network does not implement %s", lacking),
		}
	}
	return nil
}

// The sidecar the bearer's client binds, zero for none.
func configClientSidecar(clientSession *session.ClientSession) server.Id {
	if clientSession.ByJwt.ClientId == nil {
		return server.Id{}
	}
	view := model.GetSidecarIdentityByClient(clientSession.Ctx, clientSession.ByJwt.NetworkId, *clientSession.ByJwt.ClientId)
	if view == nil {
		return server.Id{}
	}
	return view.SidecarId
}

// Whether the bearer's client's sidecar stands admitted at the tip.
func configClientAdmitted(sequencer *model.ConfigLogSequencer, clientSession *session.ClientSession) (bool, error) {
	sidecarId := configClientSidecar(clientSession)
	if sidecarId == (server.Id{}) {
		return false, nil
	}
	return sequencer.Admitted(clientSession.Ctx, clientSession.ByJwt.NetworkId, sidecar.Id(sidecarId))
}

// Whether the bearer is the network's owner as base 1's genesis names it
// (§7.9's who): a personal network's admin_user_id, whose member_id is its user
// id, or in a team a live Owner membership, the genesis's member its member_id.
// A team's credential with no member_id names its owner of record, a tagged
// lineage's since QD-1194, which is never the genesis's owner.
func configGenesisOwner(clientSession *session.ClientSession, genesis *bridge.Genesis) bool {
	byJwt := clientSession.ByJwt
	if byJwt.MemberId != nil {
		signIn := model.ResolveNetworkSignIn(clientSession.Ctx, byJwt.UserId, byJwt.NetworkId, server.NowUtc())
		return signIn != nil && signIn.MemberId != nil && *signIn.MemberId == *byJwt.MemberId &&
			signIn.Role == model.NetworkMemberRoleOwner &&
			bytes.Equal(genesis.GetOwnerMemberId(), sidecar.Id(*byJwt.MemberId).Bytes())
	}
	if model.NetworkIsTeam(clientSession.Ctx, byJwt.NetworkId) {
		return false
	}
	network := model.GetNetwork(clientSession)
	return network != nil && network.AdminUserId != nil && *network.AdminUserId == byJwt.UserId &&
		bytes.Equal(genesis.GetOwnerMemberId(), sidecar.Id(*network.AdminUserId).Bytes())
}

// POST /network/config/delta: verifies the statement at the tip and appends
// it, answering {n, A_n}; a retry of the same bytes answers its entry.
func NetworkConfigDelta(args *NetworkConfigDeltaArgs, clientSession *session.ClientSession) (*NetworkConfigPinResult, error) {
	if args == nil || len(args.Signed) == 0 {
		return nil, fmt.Errorf("%d A statement is required.", http.StatusBadRequest)
	}
	sequencer, err := model.CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	admitted, err := configClientAdmitted(sequencer, clientSession)
	if err != nil {
		return nil, &model.ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: err}
	}
	if !admitted {
		signed := &bridge.Signed{}
		if proto.Unmarshal(args.Signed, signed) != nil || len(signed.GetSignerId()) != len(sidecar.Id{}) || len(signed.GetKeyId()) != sha256.Size {
			return nil, configUnadmitted("an unadmitted client posts a recovery-signed statement only")
		}
	}
	// what the log's rule cannot judge: a forwarding Delegation's host client, against api's own client rows
	// (config_delegation_check.go; D-TEAM-33)
	if err := configDelegationHostCheck(clientSession.Ctx, clientSession.ByJwt.NetworkId, args.Signed); err != nil {
		return nil, err
	}
	pin, err := sequencer.AppendStatement(clientSession.Ctx, clientSession.ByJwt.NetworkId, args.Signed)
	if err != nil {
		return nil, err
	}
	model.SyncDnsEdgeRecordsAtTip(clientSession.Ctx, sequencer, clientSession.ByJwt.NetworkId)
	return configPinResult(pin), nil
}

// POST /network/config/projection: a config writer's base, base 1 with its
// genesis from the network's first device.
func NetworkConfigProjectionPublish(args *NetworkConfigProjectionPublishArgs, clientSession *session.ClientSession) (*NetworkConfigPinResult, error) {
	if args == nil || len(args.SignedHeader) == 0 {
		return nil, fmt.Errorf("%d A signed header is required.", http.StatusBadRequest)
	}
	sequencer, err := model.CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	if args.Genesis != nil {
		// base 1: the genesis must name this network and the bearer's own
		// client's sidecar as its first device (§7.9's who); its owner being
		// the network's owner is checked where the genesis's member rows are,
		// with enrollment's owner rows (TEAM-M1-04)
		signedGenesis := &bridge.Signed{}
		if err := proto.Unmarshal(args.Genesis, signedGenesis); err != nil {
			return nil, &model.ConfigLogError{Status: http.StatusForbidden, Code: "config_refused", Err: err}
		}
		message, err := sidecar.ParseConfigStatement(bridge.SignedPurposeAuthority, bridge.SignedType_SIGNED_GENESIS, signedGenesis.GetBody())
		if err != nil {
			return nil, &model.ConfigLogError{Status: http.StatusForbidden, Code: "config_refused", Err: err}
		}
		genesis := message.(*bridge.Genesis)
		sidecarId := configClientSidecar(clientSession)
		if !bytes.Equal(genesis.GetNetworkId(), sidecar.Id(clientSession.ByJwt.NetworkId).Bytes()) ||
			sidecarId == (server.Id{}) || !bytes.Equal(genesis.GetOwnerSidecarId(), sidecar.Id(sidecarId).Bytes()) {
			return nil, configUnadmitted("base 1 comes from the device its genesis names, in its own network")
		}
		// §7.9's who: the network owner's own top-level client, the genesis naming that owner as its member: in
		// a personal network the owner's user, admin_user_id; in a team an Owner's membership, which the
		// client's credential names, never the team's synthetic owner of record (TEAM-M6-02)
		if !configGenesisOwner(clientSession, genesis) {
			return nil, configUnadmitted("base 1 comes from the network owner's device")
		}
		networkClient := model.GetNetworkClient(clientSession.Ctx, *clientSession.ByJwt.ClientId)
		if networkClient == nil || networkClient.SourceClientId != nil {
			return nil, configUnadmitted("base 1 comes from a top-level client")
		}
		// and an untagged, lasting one: enrollment refuses a tagged or ephemeral
		// client before base 1 (no_genesis), and this refuses one minted after
		// a lost base 1 too
		taggedOrEphemeral, err := model.ClientTaggedOrEphemeral(clientSession.Ctx, *clientSession.ByJwt.ClientId)
		if err != nil {
			return nil, &model.ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: err}
		}
		if taggedOrEphemeral {
			return nil, configUnadmitted("base 1 comes from an untagged device that is not ephemeral")
		}
	} else {
		admitted, err := configClientAdmitted(sequencer, clientSession)
		if err != nil {
			return nil, &model.ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: err}
		}
		if !admitted {
			return nil, configUnadmitted("an unadmitted client publishes no base")
		}
	}
	// DESIGN-COMPUTE §16.6's point row: a feature some service serving the network lacks
	if err := configBaseFeatureCheck(clientSession, sequencer, args.Rows); err != nil {
		return nil, err
	}
	pin, err := sequencer.AppendBase(clientSession.Ctx, clientSession.ByJwt.NetworkId, &model.ConfigLogBase{
		SignedHeader: args.SignedHeader,
		Rows:         args.Rows,
		Genesis:      args.Genesis,
		ConfigPushes: args.ConfigPushes,
	})
	if err != nil {
		return nil, err
	}
	model.SyncDnsEdgeRecordsAtTip(clientSession.Ctx, sequencer, clientSession.ByJwt.NetworkId)
	return configPinResult(pin), nil
}

// GET /network/config/projection?after=: the entries after the position the
// caller holds, which it verifies against its own pin.
func NetworkConfigProjection(args *NetworkConfigProjectionArgs, clientSession *session.ClientSession) (*NetworkConfigProjectionResult, error) {
	sequencer, err := model.CurrentConfigLogSequencer()
	if err != nil {
		return nil, err
	}
	read, err := sequencer.Read(clientSession.Ctx, clientSession.ByJwt.NetworkId, args.After)
	if errors.Is(err, model.ErrConfigLogGenesisMissing) {
		return nil, fmt.Errorf("%d The log's genesis is not written yet.", http.StatusServiceUnavailable)
	}
	if err != nil {
		return nil, &model.ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: err}
	}
	return &NetworkConfigProjectionResult{
		Genesis: read.SignedGenesis,
		Entries: read.Entries,
		Rows:    read.Rows,
		Tip:     configPinResult(read.Tip),
		More:    read.More,
	}, nil
}
