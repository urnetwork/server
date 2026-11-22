package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Capture the actual recorder in a child process so neighboring tests retain
// their own log destinations. Every token check is supplied by this fixture.
func TestPlayNotificationErrorStage(t *testing.T) {
	const childKey = "URNETWORK_TEST_PLAY_NOTIFICATION_STAGE"
	if os.Getenv(childKey) == "" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPlayNotificationErrorStage$", "-logtostderr=true")
		cmd.Env = append(os.Environ(), childKey+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("synthetic Play handler child failed (%v): %s", err, output)
		}
		var markers []string
		for _, line := range strings.Split(string(output), "\n") {
			if _, marker, ok := strings.Cut(line, "[store-notification]"); ok {
				markers = append(markers, marker)
			}
		}
		if len(markers) != 2 || markers[0] != "google error stage=token_info" || markers[1] != "google error stage=verified_processing" {
			t.Fatalf("unexpected Play outcome markers: %q", markers)
		}
		return
	}

	t.Cleanup(server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  publisher_email: publisher@fixture.example\n  audience: "+syntheticPlayAudience+"\n")))
	before := storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, StoreNotificationError)
	headers := map[string]string{"Authorization": "Bearer synthetic-private-token"}
	const body = `{"message":{"data":"!"}}`
	setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) {
		return nil, errors.New("synthetic-private-lookup")
	})
	if _, err := verifyWebhookBody(t, VerifyPlayBody, body, headers); err == nil {
		t.Fatal("unavailable token information passed verification")
	}
	setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) {
		return map[string]any{"email": "publisher@fixture.example", "email_verified": true, "iss": "https://accounts.google.com", "aud": syntheticPlayAudience}, nil
	})
	verified, err := verifyWebhookBody(t, VerifyPlayBody, body, headers)
	if err != nil {
		t.Fatal(err)
	}
	var args PlayWebhookArgs
	if err := json.Unmarshal(verified, &args); err != nil {
		t.Fatal(err)
	}
	if _, err := PlayWebhook(&args, &session.ClientSession{Ctx: t.Context()}); err == nil {
		t.Fatal("verified push with malformed message data was accepted")
	}
	if delta := storeNotificationCountForTest(t, model.SubscriptionMarketGoogle, StoreNotificationError) - before; delta != 2 {
		t.Fatalf("Google error counter delta = %v, want 2", delta)
	}
}
