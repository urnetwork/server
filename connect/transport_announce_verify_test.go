package connect

import (
	"bytes"
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The Connect feeder and API resolver are separate production processes but
// share one keyed Redis namespace. This reproduces the release failure where
// Connect used unkeyed model defaults while API used verify.yml's HMAC key.
func TestConnectionVerifyEgressUsesControllerHashNamespace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := model.DefaultVerifySettings()
		settings.EgressHashKey = bytes.Repeat([]byte{0x5a}, 32)
		settings.EgressHashKeyId = "transport-announce-test-v1"
		controller.SetVerifySettings(settings)
		defer controller.SetVerifySettings(model.DefaultVerifySettings())
		controller.SetStConfig(&controller.StConfig{Enabled: true})
		defer controller.SetStConfig(nil)

		clientID := server.NewId()
		ip := netip.MustParseAddr("2001:db8:5a::1")
		writerSettings, enabled := connectionVerifySettings()
		if !enabled || writerSettings == nil {
			t.Fatal("enabled verification did not supply Connect settings")
		}
		model.FeedVerifyEgress(ctx, clientID, ip, writerSettings)
		defer model.ClearVerifyEgress(ctx, clientID, ip, writerSettings)

		readerSettings := controller.VerifySettings()
		got := model.ResolveVerifyEgress(ctx, ip, readerSettings)
		if got == nil || *got != clientID {
			t.Fatalf("API namespace resolved %v, want %s", got, clientID)
		}
		if model.VerifyEgressIndexHashWithSettings(ip, writerSettings) != model.VerifyEgressIndexHashWithSettings(ip, readerSettings) {
			t.Fatal("Connect writer and API reader selected different keyed namespaces")
		}

		unkeyed := model.DefaultVerifySettings()
		if model.VerifyEgressIndexHashWithSettings(ip, writerSettings) == model.VerifyEgressIndexHashWithSettings(ip, unkeyed) {
			t.Fatal("configured and unkeyed namespaces unexpectedly alias")
		}
		if wrong := model.ResolveVerifyEgress(ctx, ip, unkeyed); wrong != nil {
			t.Fatalf("mismatched namespace attributed provider %s", *wrong)
		}
	})
}

// A deployment with the subnet disabled intentionally has no verify.yml.
// Connect must branch on feature state before attempting that vault lookup.
func TestConnectionVerifyEgressDisabledAvoidsVerifySettings(t *testing.T) {
	controller.SetStConfig(&controller.StConfig{Enabled: false})
	defer controller.SetStConfig(nil)
	controller.SetVerifySettings(nil)
	defer controller.SetVerifySettings(model.DefaultVerifySettings())

	settings, enabled := connectionVerifySettings()
	if enabled || settings != nil {
		t.Fatalf("disabled verification returned enabled=%t settings=%v", enabled, settings)
	}
}
