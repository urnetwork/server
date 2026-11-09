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
// genesis. The statements themselves are judged by their signatures and the
// log's rules (model/config_log_model.go), so this is defense in depth, not
// the authority.
//
// Refusals answer {"error", "code", "tip"}: 403 config_refused, 409
// config_tip_moved and config_unadmitted's 403; an unavailable store or a
// process with no first metadata cluster answers 503.

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
	pin, err := sequencer.AppendStatement(clientSession.Ctx, clientSession.ByJwt.NetworkId, args.Signed)
	if err != nil {
		return nil, err
	}
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
	} else {
		admitted, err := configClientAdmitted(sequencer, clientSession)
		if err != nil {
			return nil, &model.ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: err}
		}
		if !admitted {
			return nil, configUnadmitted("an unadmitted client publishes no base")
		}
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
