//go:build encryption_reset_draft

// Draft: end-to-end encryption handshake-reset scenarios.
//
// Excluded from the normal build (tag `encryption_reset_draft`). Sketches two
// integration tests for `server/connect`, where the real contract + cipher
// plumbing exists: both clients run Encrypt=true with platform-issued
// contracts, so the per-peer TLS session establishes a cipher and application
// data is outer-wrapped. Deterministic, mechanism-level versions already live
// in connect/transfer_encrypt_test.go:
//       TestHandshakeEpochResetReplacesEpoch
//       TestResetIfHandshakeIncomplete
//       TestDeliverClientHelloResetsCompletedServerHandshake
//       TestReleasedSessionRemovedFromManager
//       TestAcquireForSendRoleDispatch
//
// To finish: enable the tag and implement `setupEncryptedConnectPair` by
// factoring the server/contract/transport setup out of `testConnect`. The new
// per-scenario logic — idle-timeout-sized pauses and the resume/reject
// observation — is written out below.
//
// Run with: go test -tags encryption_reset_draft ./connect/ -run EncryptionHandshakeReset

package connect

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// encryptedPair is the minimal surface the two scenarios need from the
// server-backed harness. Implement it by extracting the relevant parts of
// testConnect.
type encryptedPair struct {
	// a is the TLS-client side of the per-peer session. Pin the IDs so
	// connect.roleForPeer makes `a` the TLS-client (lex-lower ClientId): only
	// the client role drives a fresh ClientHello on resume (scenario 1).
	a connect.Id
	b connect.Id

	// send enqueues an application message a->b (wrapped once the cipher is
	// established). Returns the Client.Send success flag.
	send func(content string) bool
	// recvB returns the next message delivered to b, or false on timeout.
	recvB func(timeout time.Duration) (string, bool)

	cleanup func()
}

// setupEncryptedConnectPair stands up two encrypted clients over the connect
// server with platform-issued contracts, parameterized by the send/receive
// idle timeouts. TODO: factor out of testConnect — reuse its network / device
// / contract / transport setup, apply the idle-timeout overrides below, and
// wire AddReceiveCallback into recvB.
func setupEncryptedConnectPair(
	t *testing.T,
	ctx context.Context,
	sendIdleTimeout time.Duration,
	receiveIdleTimeout time.Duration,
) *encryptedPair {
	t.Skip("DRAFT: implement by factoring the server/contract setup out of testConnect")

	// Sketch of the per-client settings the scenarios rely on:
	//
	//   settings := connect.DefaultClientSettings()
	//   settings.EncryptionSettings.Encrypt = true
	//   settings.EncryptionSettings.HandshakeTimeout = 60 * time.Second
	//   settings.SendBufferSettings.IdleTimeout = sendIdleTimeout
	//   settings.ReceiveBufferSettings.IdleTimeout = receiveIdleTimeout
	//   // pin client IDs so `a` is lex-lower => TLS-client (initiator)
	//
	// Both clients need Encrypt=true for the handshake to complete.
	_ = sendIdleTimeout
	_ = receiveIdleTimeout
	return nil
}

// TestEncryptionHandshakeResetOnResume is the end-to-end form of
// connect.TestDeliverClientHelloResetsCompletedServerHandshake +
// TestAcquireForSendRoleDispatch (client role).
//
// Recovery case: a->b SendSequence idle-times-out while b's ReceiveSequence
// stays alive (receive idle > send idle), keeping b's per-peer session
// established. On resume the TLS-client side starts a fresh handshake (new
// ClientHello); b's live ReceiveSequence delivers it, and the session resets
// and re-handshakes rather than dropping the ClientHello against a completed
// TLS state. The resumed, re-encrypted messages must arrive.
func TestEncryptionHandshakeResetOnResume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sendIdle := 2 * time.Second
	recvIdle := 60 * time.Second // must exceed the pause so b's receiver survives
	pair := setupEncryptedConnectPair(t, ctx, sendIdle, recvIdle)
	defer pair.cleanup()

	// 1) Establish: send a burst; all must arrive (handshake done, cipher
	//    established, data wrapped).
	for i := 0; i < 8; i += 1 {
		connect.AssertEqual(t, true, pair.send(fmt.Sprintf("pre-%d", i)))
	}
	for i := 0; i < 8; i += 1 {
		got, ok := pair.recvB(30 * time.Second)
		connect.AssertEqual(t, true, ok)
		connect.AssertEqual(t, fmt.Sprintf("pre-%d", i), got)
	}

	// 2) Pause past the send idle timeout but under the receive idle timeout:
	//    a's SendSequence times out and releases; b's ReceiveSequence (and
	//    thus b's established session) survives.
	time.Sleep(sendIdle + 2*time.Second)

	// 3) Resume: a's new SendSequence (TLS-client) resets and sends a fresh
	//    ClientHello; b's live ReceiveSequence resets its handshake and
	//    re-handshakes. The resumed messages must still arrive.
	for i := 0; i < 8; i += 1 {
		connect.AssertEqual(t, true, pair.send(fmt.Sprintf("post-%d", i)))
	}
	for i := 0; i < 8; i += 1 {
		got, ok := pair.recvB(30 * time.Second)
		connect.AssertEqual(t, true, ok)
		connect.AssertEqual(t, fmt.Sprintf("post-%d", i), got)
	}
}

// TestEncryptionReceiveTimeoutRejectsData is the end-to-end form of
// connect.TestReleasedSessionRemovedFromManager.
//
// Failure case that motivates receive_idle > send_idle. Timeouts inverted:
// receive idle < pause < send idle, so b's ReceiveSequence idle-times-out and
// releases b's session (no active handshake) while a's SendSequence stays
// live and established. a's wrapped data then hits b's fresh ReceiveSequence,
// which finds no cipher and rejects it. The post-timeout messages must not
// arrive.
func TestEncryptionReceiveTimeoutRejectsData(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Inverted vs. the recovery case: receive times out first.
	sendIdle := 60 * time.Second
	recvIdle := 2 * time.Second
	pair := setupEncryptedConnectPair(t, ctx, sendIdle, recvIdle)
	defer pair.cleanup()

	// 1) Establish.
	for i := 0; i < 8; i += 1 {
		connect.AssertEqual(t, true, pair.send(fmt.Sprintf("pre-%d", i)))
	}
	for i := 0; i < 8; i += 1 {
		got, ok := pair.recvB(30 * time.Second)
		connect.AssertEqual(t, true, ok)
		connect.AssertEqual(t, fmt.Sprintf("pre-%d", i), got)
	}

	// 2) Pause past the receive idle timeout but under the send idle timeout:
	//    b's ReceiveSequence times out and releases b's session; a's
	//    established SendSequence stays alive.
	time.Sleep(recvIdle + 2*time.Second)

	// 3) a (still established, still wrapping) sends more. b has no session
	//    for a, so the unwrap path rejects the wrapped frames; the messages
	//    must not arrive within the window. (A TLS-client `a` would
	//    re-handshake here and recover — exactly the case the config guards
	//    against by keeping receive idle > send idle.)
	for i := 0; i < 8; i += 1 {
		pair.send(fmt.Sprintf("post-%d", i))
	}
	if got, ok := pair.recvB(5 * time.Second); ok {
		t.Fatalf("expected post-timeout wrapped data to be rejected, but received %q", got)
	}
}

var _ = protocol.MessageType(0) // keep the protocol import for the implementer
