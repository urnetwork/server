package controller

// An extender network whose configuration is unusable is refused with the
// configuration error's message. Its cause can quote vault paths, the api url
// or, as here, a character of the configured root key; it stays in the log.

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A root key that is not hex: its parse error quotes the first bad character.
func installUnusableExtenderConfig(t testing.TB) {
	installTestExtenderConfigYaml(t, "root_private_key_hex: synthetic-root-key\n")
}

func newExtenderConfigRefusalSession(t testing.TB) *session.ClientSession {
	clientId := server.NewId()
	clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId(), ClientId: &clientId})
	t.Cleanup(clientSession.Cancel)
	clientSession.ClientAddress = "192.0.2.80:4500"
	return clientSession
}

func TestExtenderActivateHidesAnUnusableConfiguration(t *testing.T) {
	installUnusableExtenderConfig(t)
	result, err := ExtenderActivate(&ExtenderActivateArgs{PublicKeyHex: hex.EncodeToString(make([]byte, ed25519.PublicKeySize)), Carriers: []string{connect.ExtenderCarrierTcp}}, newExtenderConfigRefusalSession(t))
	if err != nil || result == nil || result.Activated || result.Error != "the extender root key is not readable" {
		t.Fatalf("unusable configuration was not refused with its message alone: %+v %v", result, err)
	}
}

func TestExtenderReleaseHidesAnUnusableConfiguration(t *testing.T) {
	installUnusableExtenderConfig(t)
	result, err := ExtenderRelease(&ExtenderReleaseArgs{}, newExtenderConfigRefusalSession(t))
	if err != nil || result == nil || len(result.Records) != 0 || result.Error != "the extender root key is not readable" {
		t.Fatalf("unusable configuration was not refused with its message alone: %+v %v", result, err)
	}
}
