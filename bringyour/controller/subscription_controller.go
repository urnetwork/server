package controller

import (
	"context"

	"bringyour.com/bringyour"
)



// app initially calls "get info"
// then if no wallet, show a button to initialize wallet
// if wallet, show a button to refresh, and to withdraw

// FIXME FIXME
// Get Circle UC wallet info
// Circle UC wallet initialize
// Circle UC wallet withdraw





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

