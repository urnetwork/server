package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/jwt"
	// "github.com/urnetwork/server/session"
)

func TestProductUpdates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fakeUserId := server.NewId()

		productUpdates, userEmails := GetProductUpdateUserEmailsForUser(ctx, fakeUserId)
		connect.AssertEqual(t, productUpdates, false)
		connect.AssertEqual(t, len(userEmails), 0)

		userEmailUserIds := GetUserEmailsForProductUpdatesSync(ctx)
		connect.AssertEqual(t, len(userEmailUserIds), 0)

		userIdProductUpdatesSync := map[server.Id]bool{}
		SetProductUpdatesSyncForUsers(ctx, userIdProductUpdatesSync)

		userEmailNetworkIds := GetNetworkUserEmailsForProductUpdatesSync(ctx)
		connect.AssertEqual(t, len(userEmailNetworkIds), 0)

		networkIdProductUpdatesSync := map[server.Id]bool{}
		SetNetworkProductUpdatesSyncForUsers(ctx, networkIdProductUpdatesSync)

	})
}
