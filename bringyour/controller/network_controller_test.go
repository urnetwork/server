package controller

import (
	"context"
	"testing"
	"time"

	// "golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

func TestNetworkCreate(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, nil)

		userAuth := "foo@ur.io"
		password := "bar123456789Foo!"
		networkCreate := model.NetworkCreateArgs{
			UserName:    "",
			UserAuth:    &userAuth,
			Password:    &password,
			NetworkName: "foobar",
			Terms:       true,
			GuestMode:   false,
		}
		result, err := NetworkCreate(networkCreate, session)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.Network, nil)

		transferBalances := model.GetActiveTransferBalances(ctx, result.Network.NetworkId)
		assert.Equal(t, 1, len(transferBalances))
		transferBalance := transferBalances[0]
		assert.Equal(t, transferBalance.BalanceByteCount, RefreshFreeTransferBalance)
		assert.Equal(t, !transferBalance.StartTime.After(time.Now()), true)
		assert.Equal(t, time.Now().Before(transferBalance.EndTime), true)
	})
}
