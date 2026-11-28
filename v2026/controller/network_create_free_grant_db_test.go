package controller

// The daily free data at sign-up. The owner, 2026-11-11: "sign up codes should
// still get daily free data. all accounts should get the daily free data." A
// sign-up that redeems a balance code gets the day's free grant beside the
// code's data, exactly like a sign-up without one: a balance code is data only
// (pro = false, no subscription renewal), so it never takes a network out of
// the free grant (pro.yml free.data). The daily run grants both again. Needs
// the test database and pro.yml.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// freeGrantTestGrants are the network's active daily free grants.
func freeGrantTestGrants(ctx context.Context, networkId server.Id) []*model.TransferBalance {
	grants := []*model.TransferBalance{}
	for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
		if balance.GrantKind == model.GrantKindFree {
			grants = append(grants, balance)
		}
	}
	return grants
}

// freeGrantTestSignup creates a network through the sign-up endpoint with an
// email and password, redeeming balanceCode when it is not nil.
func freeGrantTestSignup(t testing.TB, ctx context.Context, name string, balanceCode *string) *model.NetworkCreateResult {
	suffix := server.NewId().String()[:8]
	userAuth := fmt.Sprintf("%s-%s@ur.io", name, suffix)
	password := "bar123456789Foo!"
	result, err := NetworkCreate(model.NetworkCreateArgs{
		UserAuth:    &userAuth,
		Password:    &password,
		NetworkName: name + "-" + suffix,
		Terms:       true,
		BalanceCode: balanceCode,
	}, session.Testing_CreateClientSession(ctx, nil))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, nil)
	if result.Network == nil {
		t.Fatalf("sign-up %s made no network: %+v", name, result)
	}
	return result
}

func TestSignupFreeGrantWithBalanceCode(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		messages := newOutboxTestSender()
		previousSender := GetAWSMessageSender()
		SetMessageSender(messages)
		defer SetMessageSender(previousSender)

		codeByteCount := model.ByteCount(1024 * 1024 * 1024 * 1024)
		balanceCode, err := model.CreateBalanceCode(ctx, codeByteCount, 365*24*time.Hour, model.UsdToNanoCents(3), "", "", "")
		connect.AssertEqual(t, err, nil)

		withCode := freeGrantTestSignup(t, ctx, "signupcode", &balanceCode.Secret)
		withoutCode := freeGrantTestSignup(t, ctx, "signupplain", nil)

		freeData := model.Pro().DataAmount(false)
		for _, signup := range []struct {
			name      string
			networkId server.Id
		}{
			{"with a balance code", withCode.Network.NetworkId},
			{"without a balance code", withoutCode.Network.NetworkId},
		} {
			grants := freeGrantTestGrants(ctx, signup.networkId)
			if len(grants) != 1 {
				t.Fatalf("sign-up %s: %d daily free grants, want the day's one", signup.name, len(grants))
			}
			grant := grants[0]
			connect.AssertEqual(t, grant.StartBalanceByteCount, freeData)
			connect.AssertEqual(t, grant.Pro, false)
			startTime, endTime := model.FreeGrantWindow(grant.StartTime)
			connect.AssertEqual(t, grant.StartTime.Equal(startTime), true)
			connect.AssertEqual(t, grant.EndTime.Equal(endTime), true)
		}

		// the code's data is there beside the free grant, and the code did not
		// make the network Pro
		codeData := model.ByteCount(0)
		for _, balance := range model.GetActiveTransferBalances(ctx, withCode.Network.NetworkId) {
			connect.AssertEqual(t, balance.Pro, false)
			if balance.GrantKind == model.GrantKindNone && balance.Paid {
				codeData += balance.StartBalanceByteCount
			}
		}
		connect.AssertEqual(t, codeData, codeByteCount)
		connect.AssertEqual(t, model.IsPro(ctx, &withCode.Network.NetworkId), false)

		// the daily run grants both networks again
		startTime, endTime := model.FreeGrantWindow(server.NowUtc())
		added := model.AddFreeTransferBalanceToAllNetworks(ctx, startTime, endTime, freeData)
		connect.AssertEqual(t, added[withCode.Network.NetworkId], freeData)
		connect.AssertEqual(t, added[withoutCode.Network.NetworkId], freeData)
	})
}
