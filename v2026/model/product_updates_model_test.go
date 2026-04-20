package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2026"
	// "github.com/urnetwork/server/v2026/jwt"
	// "github.com/urnetwork/server/v2026/session"
)

func TestProductUpdates(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fakeUserId := server.NewId()

		productUpdates, userEmails := GetProductUpdateUserEmailsForUser(ctx, fakeUserId)
		assert.Equal(t, productUpdates, false)
		assert.Equal(t, len(userEmails), 0)

		userEmailUserIds := GetUserEmailsForProductUpdatesSync(ctx)
		assert.Equal(t, len(userEmailUserIds), 0)

		userIdProductUpdatesSync := map[server.Id]bool{}
		SetProductUpdatesSyncForUsers(ctx, userIdProductUpdatesSync)

		userEmailNetworkIds := GetNetworkUserEmailsForProductUpdatesSync(ctx)
		assert.Equal(t, len(userEmailNetworkIds), 0)

		networkIdProductUpdatesSync := map[server.Id]bool{}
		SetNetworkProductUpdatesSyncForUsers(ctx, networkIdProductUpdatesSync)

	})
}
