//go:build !sidecar_pending

package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
)

// A release leg until MG1's Gn-out: no auth key table exists, so each auth key
// route answers that auth keys are not available, nothing consumes, and no
// statement reaches the missing table. API keys stay legacy without MG0: one
// asking for scopes or an expiry is refused rather than made without them.
func TestAuthKeysAreUnavailableWithoutMg1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, clientSession := authParityAccount(t, ctx, "authkey-release")
		if server.SidecarMg1Linked() || server.SidecarMg0Linked() {
			t.Fatal("a release build links a pending group")
		}

		if _, err := CreateAuthKey(&CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 1}, clientSession); err == nil || !strings.HasPrefix(err.Error(), "503 ") {
			t.Fatalf("create: %v", err)
		}
		if _, err := GetNetworkAuthKeys(clientSession); err == nil || !strings.HasPrefix(err.Error(), "503 ") {
			t.Fatalf("list: %v", err)
		}
		if _, err := RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: server.NewId()}, clientSession); err == nil || !strings.HasPrefix(err.Error(), "503 ") {
			t.Fatalf("revoke: %v", err)
		}
		bearer := "ur-authkey-" + strings.Repeat("a", 26) + "-" + strings.Repeat("a", 52)
		server.Tx(ctx, func(tx server.PgTx) {
			if _, err := ConsumeAuthKeyInTx(ctx, tx, bearer); !errors.Is(err, ErrAuthKeyUnknown) {
				t.Fatalf("consume: %v", err)
			}
			if _, err := ConsumeAuthKeyInTx(ctx, tx, bearer+"#bundle"); !errors.Is(err, ErrAuthKeyHoldsBundle) {
				t.Fatalf("consume with a bundle: %v", err)
			}
		})
		if removed := RemoveExpiredNetworkAuthKeys(ctx, server.NowUtc()); removed != 0 {
			t.Fatalf("removed %d", removed)
		}

		result, err := CreateApiKey(&CreateApiKeyArgs{Name: "scoped", Scopes: []string{ApiKeyScopeAll}}, clientSession)
		if err != nil || result.Error == nil {
			t.Fatalf("a scoped key without MG0: %+v %v", result, err)
		}
		legacy, err := CreateApiKey(&CreateApiKeyArgs{Name: "legacy"}, clientSession)
		if err != nil || legacy.Error != nil || legacy.Scopes != nil || legacy.ExpireTime != nil {
			t.Fatalf("a legacy key without MG0: %+v %v", legacy, err)
		}
		if network := apikey.GetNetworkByApiKey(legacy.ApiKey, ctx); network == nil {
			t.Fatal("a legacy key does not authenticate")
		}
	})
}
