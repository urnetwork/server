package model

import (
	"context"
	"fmt"
	"testing"

	"bringyour.com/bringyour"
	"github.com/go-playground/assert/v2"
)

func TestNetworkUser(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := bringyour.NewId()
		userId := bringyour.NewId()

		Testing_CreateNetwork(ctx, networkId, "a", userId)

		networkUser := GetNetworkUser(ctx, userId)

		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, networkUser.UserId, userId)
		assert.Equal(t, networkUser.UserName, "test")
		assert.Equal(t, networkUser.UserAuth, fmt.Sprintf("%s@bringyour.com", networkId))
		assert.Equal(t, networkUser.Verified, true)
		assert.Equal(t, networkUser.AuthType, AuthTypePassword)

		// test for invalid user id
		userId = bringyour.NewId()
		networkUser = GetNetworkUser(ctx, userId)
		assert.Equal(t, networkUser, nil)

	})
}