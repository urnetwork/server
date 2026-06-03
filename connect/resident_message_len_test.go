package main

import (
	"bytes"
	"net"
	"testing"

	"github.com/urnetwork/connect"
)

// TestResidentAdmitsMinimumMessageLenLimit verifies that an exactly
// `MinimumMessageLenLimit()`-byte message passes through the resident
// exchange's framing layer built from default settings.
//
// `MinimumMessageLenLimit()` is the worst-case single-pack wire size of the
// per-peer encryption handshake's TLS server flight, which every framer along
// the resident exchange flow must admit. This guards `DefaultConnectHandlerSettings`
// passing it as the framer max. `DefaultFramerSettings` has no global default,
// so each context sets its own cap; too small a cap would reject the handshake
// pack here ("Max message len exceeded"), `SendSequence` would retransmit it
// forever, and the per-peer session would deadlock.
//
// `ExchangeSettings` embeds `ConnectHandlerSettings`, so its `FramerSettings`
// is the same object backing the connect-handler framer and the websocket
// read limit; testing the resident `ExchangeBuffer` here covers all three.
func TestResidentAdmitsMinimumMessageLenLimit(t *testing.T) {
	settings := DefaultExchangeSettings()

	minLen := int(connect.DefaultClientSettings().MinimumMessageLenLimit())

	// Invariant: the resident exchange framer cap admits the connect runtime
	// minimum message length.
	if settings.FramerSettings.MaxMessageLen < minLen {
		t.Fatalf(
			"resident exchange framer MaxMessageLen %d < MinimumMessageLenLimit %d",
			settings.FramerSettings.MaxMessageLen,
			minLen,
		)
	}

	// Functional: a `minLen`-byte message must round-trip unchanged through the
	// resident's ExchangeBuffer framing path (resident-to-resident forwarding
	// I/O) without a framer error.
	sendBuffer := NewDefaultExchangeBuffer(settings)
	receiveBuffer := NewDefaultExchangeBuffer(settings)

	connWrite, connRead := net.Pipe()
	defer connWrite.Close()
	defer connRead.Close()

	sent := make([]byte, minLen)
	for i := range sent {
		sent[i] = byte(i % 251)
	}
	// Independent copy: WriteMessage returns its input to the message pool on
	// success, so `sent` must not be read afterward.
	expected := make([]byte, minLen)
	copy(expected, sent)

	// net.Pipe is unbuffered and the framer splits a message this size into two
	// writes, so write must run concurrently with read.
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- sendBuffer.WriteMessage(connWrite, sent)
	}()

	received, err := receiveBuffer.ReadMessage(connRead)
	if err != nil {
		t.Fatalf("ReadMessage failed for %d-byte message: %s", minLen, err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("WriteMessage failed for %d-byte message: %s", minLen, err)
	}

	if len(received) != minLen {
		t.Fatalf("received %d bytes, want %d", len(received), minLen)
	}
	if !bytes.Equal(received, expected) {
		t.Fatalf("received message bytes do not match sent message")
	}
}
