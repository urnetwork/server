package controller

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"strings"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/model"
)

// The camouflage key of an activation (connect/EXTENDER.md P1, P6;
// connect/EXTENDER-UPDATE2.md §3, phase 14b).
//
// An extender with camouflage on posts the X25519 static public key it derives
// from its identity seed, and the operator signs it into every record of the
// extender, so a client fronts the tcp carrier with an authenticated hello. The
// operator signs it only:
//
//   - after the activation passes. The passing activation's own upsert stores
//     the key, and a refused activation stores nothing.
//   - while the network that posted it holds the identity key. The key is
//     stored with the posting network, and a reader returns it only while that
//     is the key's network. An activation never moves a key to another network
//     (model.ActivateNetworkExtender). A key that reaches another network some
//     other way carries no camouflage key until that network activates it.
//
// Nothing here proves the extender holds the private half. In Phase A the
// probes pass over the legacy attempt whatever the key, and challenge binding
// is a separate proposal. A wrong key costs only that extender's camouflaged
// attempts: in Phase A they fall to the legacy attempt, and in Phase B the
// extender's own activation probe fails.

// The camouflage key an activation posts, nil for none: a 32-byte X25519
// public key a client can agree a key with. A low-order point, for which every
// client's key agreement fails, is refused like a malformed key.
func parseExtenderActivateRealityPublicKey(realityPublicKeyHex string) ([]byte, error) {
	if strings.TrimSpace(realityPublicKeyHex) == "" {
		return nil, nil
	}
	realityPublicKey, err := connect.ParseExtenderRealityPublicKeyHex(realityPublicKeyHex)
	if err != nil {
		return nil, err
	}
	publicKey, err := ecdh.X25519().NewPublicKey(realityPublicKey)
	if err != nil {
		return nil, err
	}
	// a low-order point makes every shared secret zero, which the key
	// agreement refuses. Any scalar shows it, since clamping clears the
	// small-order part.
	probeKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if _, err := probeKey.ECDH(publicKey); err != nil {
		return nil, errors.New("the key is a low-order point")
	}
	return realityPublicKey, nil
}

// ProbeExtenderUptime is the uptime probe of one address (C3): the tcp
// carrier's challenge under the extender's published key. The dial fronts its
// camouflaged attempt with the key the extender's records carry, so a splicing
// (Phase B) extender passes. Either attempt passing passes the probe, and an
// extender whose records carry no key is probed as before.
func ProbeExtenderUptime(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	serverName string,
	destinationHost string,
) error {
	_, err := connect.ProbeExtenderCarrierWithCamouflageKey(
		ctx,
		ExtenderProbeConnectSettings(),
		target.Ip,
		connect.ExtenderConnectModeTcpTls,
		target.TcpPort,
		target.DnsTld,
		serverName,
		target.PublicKey,
		target.RealityPublicKey,
		destinationHost,
		ExtenderProbeDestinationPort,
	)
	return err
}
