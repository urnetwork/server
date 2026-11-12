//go:build sidecar_pending

package model

// §7.4's forwarding client (TEAM-M8-02; owner decision 197), which needs MG8: S's own bound client mints F with its
// proof and the VPN component's ClientKey; F is strict_exp and admin_refused, its children inherit the mark, its key is
// its proof key, a repeat for the same key reissues F, and only the newest F of S's current client speaks for S.

import (
	"bytes"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A network with S's own client, strict_exp and bound to S, and a session of its token.
type forwardOnlyFixture struct {
	*clientOwnerFixture
	sidecarId      server.Id
	sidecarClient  *AuthNetworkClientResult
	sidecarSession *session.ClientSession
}

func newForwardOnlyFixture(t testing.TB) *forwardOnlyFixture {
	t.Helper()
	owner := newClientOwnerFixture(t)
	owner.pushPreview(t)
	self := &forwardOnlyFixture{clientOwnerFixture: owner, sidecarId: server.NewId()}
	self.sidecarClient = owner.mint(t, owner.root, &AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true})
	record := testSidecarBindingRecord(t, owner.networkId, self.sidecarId, testSidecarKey(t), 1)
	record.ClientId = *self.sidecarClient.ClientId
	if outcome, _ := BindSidecarIdentity(owner.ctx, record, server.NowUtc()); outcome != SidecarBindAppended {
		t.Fatalf("the binding: %s", outcome)
	}
	self.sidecarSession = owner.sessionOf(t, self.sidecarClient)
	return self
}

// A forwarding mint by S's own client.
func (self *forwardOnlyFixture) forwardArgs(key []byte) *AuthNetworkClientArgs {
	return &AuthNetworkClientArgs{
		SourceClientId:  self.sidecarClient.ClientId,
		Description:     "forwarding",
		DeviceSpec:      "test",
		AdminRefused:    true,
		StrictExp:       true,
		ForwardOnly:     true,
		ClientPublicKey: key,
	}
}

// The client's stored forward_only and client_public_key.
func (self *forwardOnlyFixture) forwardRow(t testing.TB, clientId server.Id) (forwardOnly bool, clientPublicKey []byte) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			self.ctx,
			`SELECT COALESCE(forward_only, false), client_public_key FROM network_client_owner WHERE client_id = $1`,
			clientId,
		).Scan(&forwardOnly, &clientPublicKey))
	})
	return
}

// The mint refused with message, nothing written.
func (self *forwardOnlyFixture) requireRefused(t testing.TB, name string, caller *session.ClientSession, args *AuthNetworkClientArgs, message string) {
	t.Helper()
	result, err := AuthNetworkClient(args, caller)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.Error == nil || result.Error.Message != message || result.ClientId != nil && args.ClientId == nil {
		t.Fatalf("%s: %+v %+v, want %q", name, result, result.Error, message)
	}
}

// F's mint: both marks with forward_only, its key stored as its proof key, a repeat for the same key reissuing F, a
// new key minting a newer F that alone speaks for S, F's child inheriting the mark and speaking for none, and an
// admin_refused child of S without the mark unchanged.
func TestForwardingClientMint(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newForwardOnlyFixture(t)
		Testing_ForgetClientForwardOnly()
		key := testSidecarKey(t)

		forwarding := self.mint(t, self.sidecarSession, self.forwardArgs(key))
		connect.AssertEqual(t, forwarding.AdminRefused, true)
		connect.AssertEqual(t, forwarding.StrictExp, true)
		forwardOnly, storedKey := self.forwardRow(t, *forwarding.ClientId)
		connect.AssertEqual(t, forwardOnly, true)
		connect.AssertEqual(t, bytes.Equal(storedKey, key), true)
		marked, err := ClientForwardOnly(self.ctx, self.networkId, *forwarding.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, marked, true)
		marked, err = ClientForwardOnly(self.ctx, self.networkId, *self.sidecarClient.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, marked, false)
		proofKey, err := ClientProofPublicKey(self.ctx, *forwarding.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, bytes.Equal(proofKey, key), true)
		byJwt := requireTokenLifetime(t, self.clientOwnerFixture, *forwarding.ByClientJwt, strictExpLifetime)
		connect.AssertEqual(t, byJwt.StrictExp, true)
		sidecarId, err := ForwardingClientSidecar(self.ctx, self.networkId, *forwarding.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *sidecarId, self.sidecarId)

		// a lost answer's repeat for the same key reissues F
		repeat := self.mint(t, self.sidecarSession, self.forwardArgs(key))
		connect.AssertEqual(t, *repeat.ClientId, *forwarding.ClientId)

		// a later setup's new key: a newer F, which alone speaks for S; ids are
		// monotonic, so the newer one sorts last even within one millisecond
		newer := self.mint(t, self.sidecarSession, self.forwardArgs(testSidecarKey(t)))
		if *newer.ClientId == *forwarding.ClientId {
			t.Fatal("a new key reissued the old forwarding client")
		}
		sidecarId, err = ForwardingClientSidecar(self.ctx, self.networkId, *newer.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *sidecarId, self.sidecarId)
		sidecarId, err = ForwardingClientSidecar(self.ctx, self.networkId, *forwarding.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, sidecarId == nil, true)

		// F's own child, a window client, inherits the mark and speaks for no sidecar
		forwardingSession := self.sessionOf(t, newer)
		window := self.mint(t, forwardingSession, &AuthNetworkClientArgs{SourceClientId: newer.ClientId, Description: "window", DeviceSpec: "test"})
		Testing_ForgetClientForwardOnly()
		marked, err = ClientForwardOnly(self.ctx, self.networkId, *window.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, marked, true)
		sidecarId, err = ForwardingClientSidecar(self.ctx, self.networkId, *window.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, sidecarId == nil, true)

		// an admin_refused child of S without the mark is unchanged
		plain := self.mint(t, self.sidecarSession, &AuthNetworkClientArgs{SourceClientId: self.sidecarClient.ClientId, Description: "child", DeviceSpec: "test", AdminRefused: true})
		marked, err = ClientForwardOnly(self.ctx, self.networkId, *plain.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, marked, false)
		sidecarId, err = ForwardingClientSidecar(self.ctx, self.networkId, *plain.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, sidecarId == nil, true)
	})
}

// A forwarding mint is refused unless S's own bound client asks for its own child with a 32-byte key; a key without
// the mark is refused; a reissue cannot add the mark or change the key.
func TestForwardingClientMintRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newForwardOnlyFixture(t)
		key := testSidecarKey(t)

		self.requireRefused(t, "by the network's root token", self.root, self.forwardArgs(key), clientForwardOnlyMinterMessage)
		otherSource := self.forwardArgs(key)
		otherClient := server.NewId()
		otherSource.SourceClientId = &otherClient
		self.requireRefused(t, "for another source", self.sidecarSession, otherSource, clientForwardOnlyMinterMessage)
		self.requireRefused(t, "with a short key", self.sidecarSession, self.forwardArgs(key[:16]), clientForwardOnlyKeyMessage)
		self.requireRefused(t, "with no key", self.sidecarSession, self.forwardArgs(nil), clientForwardOnlyKeyMessage)
		keyAlone := &AuthNetworkClientArgs{SourceClientId: self.sidecarClient.ClientId, Description: "child", DeviceSpec: "test", ClientPublicKey: key}
		self.requireRefused(t, "a key with no mark", self.sidecarSession, keyAlone, clientPublicKeyMarkMessage)

		unbound := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "unbound", DeviceSpec: "test", StrictExp: true})
		unboundArgs := self.forwardArgs(key)
		unboundArgs.SourceClientId = unbound.ClientId
		self.requireRefused(t, "from an unbound client", self.sessionOf(t, unbound), unboundArgs, clientForwardOnlyUnboundMessage)

		addMark := &AuthNetworkClientArgs{ClientId: self.sidecarClient.ClientId, Description: "sidecar", DeviceSpec: "test", StrictExp: true, ForwardOnly: true}
		self.requireRefused(t, "a reissue adding the mark", self.sidecarSession, addMark, clientMarksImmutableMessage)

		forwarding := self.mint(t, self.sidecarSession, self.forwardArgs(key))
		otherKey := self.forwardArgs(testSidecarKey(t))
		otherKey.ClientId = forwarding.ClientId
		self.requireRefused(t, "a reissue naming another key", self.sidecarSession, otherKey, clientPublicKeyMismatchMessage)
	})
}
