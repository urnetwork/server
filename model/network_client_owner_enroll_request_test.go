//go:build sidecar_pending

package model

// POST /network/enroll's request bound at the mint (NetworkEnrollRequestSettings, QD-702), on Postgres with MG1: the
// longest device_spec is the settings' field, read from the instance the enrollment is given.

import (
	"crypto/ed25519"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A device_spec at the bound passes to the key's read, where a key no network holds answers 401 auth_key_invalid,
// and one a byte past it answers 400 bad_request before any read: the default's 256, the width of
// network_client.device_spec, and a test's 8.
func TestNetworkEnrollDeviceSpecBoundIsASetting(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		publicKey, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		// a well-formed bearer of a key no network holds
		bearer := authKeyBearer(server.NewId(), make([]byte, authKeySecretByteCount))
		enroll := func(settings *NetworkEnrollRequestSettings, deviceSpecLength int) error {
			_, err := enrollNetworkClientWithSettings(ctx, &NetworkEnrollArgs{
				AuthKey:         bearer,
				ClientPublicKey: publicKey,
				DeviceSpec:      strings.Repeat("d", deviceSpecLength),
			}, "192.0.2.9:1", settings)
			return err
		}

		defaults := DefaultNetworkEnrollRequestSettings()
		if defaults.DeviceSpecMaxLength != 256 {
			t.Fatalf("the default device_spec bound is %d", defaults.DeviceSpecMaxLength)
		}
		lowered := DefaultNetworkEnrollRequestSettings()
		lowered.DeviceSpecMaxLength = 8
		for _, settings := range []*NetworkEnrollRequestSettings{defaults, lowered} {
			requireTeamEnrollRefusal(t, enroll(settings, settings.DeviceSpecMaxLength), http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
			requireTeamEnrollRefusal(t, enroll(settings, settings.DeviceSpecMaxLength+1), http.StatusBadRequest, NetworkEnrollCodeBadRequest)
		}
	})
}
