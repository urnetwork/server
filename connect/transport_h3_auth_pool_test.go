// H3 authentication ownership tests keep the server's first framed read
// independent of database-backed transport integration.
package connect

import (
	"bytes"
	"errors"
	"testing"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Verifies valid H3 auth bytes stay borrowed only for the callback and return
// on both its success and error paths.
func TestConnectQuicAuthFrameReturnsPoolBufferAfterUse(t *testing.T) {
	useErr := errors.New("injected auth use error")
	for caseIndex, callbackErr := range []error{nil, useErr} {
		framer := clientconnect.NewFramer(clientconnect.DefaultFramerSettings(1024))
		authFrameBytes, err := clientconnect.EncodeFrame(
			&protocol.Auth{
				ByJwt:      "testing-auth",
				AppVersion: "testing-app",
				InstanceId: []byte{1, 2, 3, 4},
			},
			clientconnect.DefaultProtocolVersion,
		)
		if err != nil {
			t.Fatal(err)
		}
		var stream bytes.Buffer
		if err := framer.Write(&stream, authFrameBytes); err != nil {
			clientconnect.MessagePoolReturn(authFrameBytes)
			t.Fatal(err)
		}
		clientconnect.MessagePoolReturn(authFrameBytes)
		callbackCount := 0
		var authWitness []byte
		err = withObservedConnectQuicAuthFrame(
			framer,
			&stream,
			func(authFrameBytes []byte) {
				authWitness = clientconnect.MessagePoolShareReadOnly(authFrameBytes)
			},
			func(auth *protocol.Auth, borrowedFrameBytes []byte) error {
				callbackCount += 1
				if auth.ByJwt != "testing-auth" || auth.AppVersion != "testing-app" {
					return errors.New("decoded auth changed")
				}
				pooled, _ := clientconnect.MessagePoolCheck(borrowedFrameBytes)
				if !pooled {
					return errors.New("auth callback did not borrow a checked-out pool buffer")
				}
				return callbackErr
			},
		)
		if callbackCount != 1 {
			t.Fatalf("case %d callback count=%d, want 1", caseIndex, callbackCount)
		}
		if callbackErr == nil && err != nil {
			t.Fatalf("case %d auth use: %v", caseIndex, err)
		}
		if callbackErr != nil && !errors.Is(err, callbackErr) {
			t.Fatalf("case %d auth error=%v, want %v", caseIndex, err, callbackErr)
		}
		if authWitness == nil || !clientconnect.MessagePoolReturn(authWitness) {
			t.Fatalf("case %d H3 auth owner outlived callback return", caseIndex)
		}
	}
}

// A framed payload that fails protocol decoding still releases the pool buffer
// borrowed by the first H3 read and never reaches authentication use.
func TestConnectQuicAuthFrameReturnsPoolBufferOnDecodeFailure(t *testing.T) {
	framer := clientconnect.NewFramer(clientconnect.DefaultFramerSettings(1024))
	var stream bytes.Buffer
	if err := framer.Write(&stream, []byte("not a protocol frame")); err != nil {
		t.Fatal(err)
	}
	callbackCalled := false
	var authWitness []byte
	err := withObservedConnectQuicAuthFrame(
		framer,
		&stream,
		func(authFrameBytes []byte) {
			authWitness = clientconnect.MessagePoolShareReadOnly(authFrameBytes)
		},
		func(*protocol.Auth, []byte) error {
			callbackCalled = true
			return nil
		},
	)
	if err == nil {
		t.Fatal("invalid H3 auth frame decoded successfully")
	}
	if callbackCalled {
		t.Fatal("invalid H3 auth frame reached the use callback")
	}
	if authWitness == nil || !clientconnect.MessagePoolReturn(authWitness) {
		t.Fatal("invalid H3 auth owner outlived decode failure")
	}
}
