package controller

import (
	"context"
	"time"

	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour"
)



// app initially calls "get info"
// then if no wallet, show a button to initialize wallet
// if wallet, show a button to refresh, and to withdraw


type SubscriptionBalanceResult struct {
	BalanceByteCount model.ByteCount `json:"balance_byte_count"`
	CurrentSubscription *model.Subscription `json:"current_subscription,omitempty"`
	ActiveTransferBalances []*model.TransferBalance `json:"active_transfer_balances,omitempty"`
	PendingPayoutUsdNanoCents model.NanoCents `json:"pending_payout_usd_nano_cents"`
	WalletInfo *CircleWalletInfo `json:"wallet_info,omitempty"`
	UpdateTime time.Time `json:"update_time"`
}


func SubscriptionBalance(session *session.ClientSession) (*SubscriptionBalanceResult, error) {
	transferBalances := model.GetActiveTransferBalances(session.Ctx, session.ByJwt.NetworkId)

	netBalanceByteCount := model.ByteCount(0)
	for _, transferBalance := range transferBalances {
		netBalanceByteCount += transferBalance.BalanceByteCount
	}

	currentSubscription := model.CurrentSubscription(session.Ctx, session.ByJwt.NetworkId)

	pendingPayout := model.GetNetPendingPayout(session.Ctx, session.ByJwt.NetworkId)

	// ignore any error with circle, 
	// since the model won't allow the wallet to enter a corrupt state
	walletInfo, _ := findMostRecentCircleWallet(session)

	return &SubscriptionBalanceResult{
		BalanceByteCount: netBalanceByteCount,
		CurrentSubscription: currentSubscription,
		ActiveTransferBalances: transferBalances,
		PendingPayoutUsdNanoCents: pendingPayout,
		WalletInfo: walletInfo,
		UpdateTime: time.Now(),
	}, nil
}


// run this every 15 minutes
// circle.yml
func AutoPayout() {
	// auto accept payout as long as it is below an amount
	// otherwise require manual processing
	// FIXME use circle
}


// call from api
func SetPayoutWallet(ctx context.Context, networkId bringyour.Id, walletId bringyour.Id) {
	
}



// coinbase callback
// stripe callback



// https://api.bringyour.com/stripe/coinbase
func StripeWebhook() {


	// notifyBalanceCode()
}


// https://api.bringyour.com/pay/coinbase
func CoinbaseWebhook() {

	// notifyBalanceCode()
}


// notification_count
// next_notify_time
func BalanceCodeNotify() {
	// in a loop get all unredeemed balance codes where next notify time is null or >= now
	// send a reminder that the customer has a balance code, and embed the code in the email
	// 1 day, 7 days, 30 days, 90 days (final reminder)
}

func notifyBalanceCode(balanceCodeId bringyour.Id) {

}

