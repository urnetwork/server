//go:build encryption_reset_draft

// DRAFT — end-to-end encryption handshake-reset scenarios.
//
// This file is intentionally excluded from the normal build (build tag
// `encryption_reset_draft`). It sketches the two integration tests that
// belong here in `server/connect`, where the real contract + cipher
// plumbing exists (both clients run Encrypt=true with platform-issued
// contracts, so the per-peer TLS session actually establishes a cipher and
// application data is outer-wrapped). The deterministic, mechanism-level
// versions already live in the `connect` package:
//
//   - connect/transfer_encrypt_test.go:
//       TestHandshakeEpochResetReplacesEpoch
//       TestResetIfHandshakeIncomplete
//       TestDeliverClientHelloResetsCompletedServerHandshake
//       TestReleasedSessionRemovedFromManager
//       TestAcquireForSendRoleDispatch
//
// To finish these drafts, enable the tag and implement
// `setupEncryptedConnectPair` by factoring the server/contract/transport
// setup out of `testConnect` (networks, devices, contracts, route manager,
// transports). The NEW logic each scenario needs — idle-timeout-sized
// pauses and the resume/reject observation — is written out below.
//
// Run with: go test -tags encryption_reset_draft ./connect/ -run EncryptionHandshakeReset

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// encryptedPair is the minimal surface the two scenarios need from the
// (heavy) server-backed harness. Implement it by extracting the relevant
// parts of testConnect.
type encryptedPair struct {
	// a is the TLS-client side of the per-peer session. Pin the client IDs
	// so connect.roleForPeer makes `a` the TLS-client (lex-lower ClientId):
	// only the client role drives a fresh ClientHello on resume, which is
	// what scenario 1 exercises.
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
// idle timeouts. TODO: factor this out of testConnect — reuse its network /
// device / contract / transport setup, then apply the idle-timeout overrides
// below and wire AddReceiveCallback into recvB.
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
	// Both clients must be Encrypt=true for the handshake to complete.
	_ = sendIdleTimeout
	_ = receiveIdleTimeout
	return nil
}

// TestEncryptionHandshakeResetOnResume is the end-to-end form of
// connect.TestDeliverClientHelloResetsCompletedServerHandshake +
// TestAcquireForSendRoleDispatch (client role).
//
// Scenario (the recovery case): the SendSequence a->b idle-times-out while
// b's ReceiveSequence stays alive (receive idle timeout > send idle timeout),
// keeping b's per-peer session established. When a resumes sending, the
// TLS-client side starts a fresh handshake (new ClientHello); b's still-live
// ReceiveSequence delivers it and the session resets and re-handshakes,
// instead of dropping the ClientHello against a completed TLS state. The
// resumed, re-encrypted messages must arrive.
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

	// 1) Establish: send a burst; everything must arrive (handshake done,
	//    cipher established, data wrapped).
	for i := 0; i < 8; i += 1 {
		assert.Equal(t, true, pair.send(fmt.Sprintf("pre-%d", i)))
	}
	for i := 0; i < 8; i += 1 {
		got, ok := pair.recvB(30 * time.Second)
		assert.Equal(t, true, ok)
		assert.Equal(t, fmt.Sprintf("pre-%d", i), got)
	}

	// 2) Pause longer than the send idle timeout but shorter than the
	//    receive idle timeout: a's SendSequence times out and releases;
	//    b's ReceiveSequence (and thus b's established session) survives.
	time.Sleep(sendIdle + 2*time.Second)

	// 3) Resume: a's new SendSequence (TLS-client) resets and sends a fresh
	//    ClientHello; b's live ReceiveSequence resets its handshake (point b)
	//    and re-handshakes. The resumed messages must still arrive.
	for i := 0; i < 8; i += 1 {
		assert.Equal(t, true, pair.send(fmt.Sprintf("post-%d", i)))
	}
	for i := 0; i < 8; i += 1 {
		got, ok := pair.recvB(30 * time.Second)
		assert.Equal(t, true, ok)
		assert.Equal(t, fmt.Sprintf("post-%d", i), got)
	}
}

// TestEncryptionReceiveTimeoutRejectsData is the end-to-end form of
// connect.TestReleasedSessionRemovedFromManager.
//
// Scenario (the failure case that motivates receive_idle > send_idle): the
// ReceiveSequence on b idle-times-out while a's SendSequence stays live (here
// the timeouts are inverted: receive idle timeout < the pause < send idle
// timeout). b's session is released, so b has no active handshake. When a's
// still-live, still-established SendSequence sends wrapped data, b's fresh
// ReceiveSequence finds no cipher and rejects it. The post-timeout messages
// must NOT arrive.
func TestEncryptionReceiveTimeoutRejectsData(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Inverted relationship vs. the recovery case: receive times out first.
	sendIdle := 60 * time.Second
	recvIdle := 2 * time.Second
	pair := setupEncryptedConnectPair(t, ctx, sendIdle, recvIdle)
	defer pair.cleanup()

	// 1) Establish.
	for i := 0; i < 8; i += 1 {
		assert.Equal(t, true, pair.send(fmt.Sprintf("pre-%d", i)))
	}
	for i := 0; i < 8; i += 1 {
		got, ok := pair.recvB(30 * time.Second)
		assert.Equal(t, true, ok)
		assert.Equal(t, fmt.Sprintf("pre-%d", i), got)
	}

	// 2) Pause longer than the receive idle timeout but shorter than the
	//    send idle timeout: b's ReceiveSequence times out and releases b's
	//    session; a's established SendSequence stays alive.
	time.Sleep(recvIdle + 2*time.Second)

	// 3) a (still established, still wrapping) sends more. b has no session
	//    for a and cannot decrypt -> the wrapped frames are rejected by the
	//    unwrap path. The messages must NOT be delivered within the window.
	//    (Note: a TLS-client `a` would re-handshake here and recover; this
	//    scenario is specifically the case the config guards against by
	//    keeping the receive idle timeout larger than the send idle timeout.)
	for i := 0; i < 8; i += 1 {
		pair.send(fmt.Sprintf("post-%d", i))
	}
	if got, ok := pair.recvB(5 * time.Second); ok {
		t.Fatalf("expected post-timeout wrapped data to be rejected, but received %q", got)
	}
}

var _ = protocol.MessageType(0) // keep the protocol import for the implementer
