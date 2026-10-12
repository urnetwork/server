// The one-time sn-reconcile census reads the payout wallets' own Circle
// history. A separate interface keeps the existing CircleApi fakes compiling.
package controller

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server/model"
)

type CirclePayoutLedger interface {
	// Every transaction of one developer wallet created at or after `from`, in
	// every state. An incomplete listing is an error, never a short result.
	ListWalletTransactions(ctx context.Context, walletId string, from time.Time) ([]CircleTransaction, error)
	// The wallet's balance of the token at `tokenAddress`; zero when absent.
	GetWalletTokenBalance(ctx context.Context, walletId string, tokenAddress string) (model.NanoCents, error)
}

var circlePayoutLedgerInstance CirclePayoutLedger = &CoreCircleApiClient{}

func NewCirclePayoutLedger() CirclePayoutLedger {
	return circlePayoutLedgerInstance
}

// for stubbing in tests
func SetCirclePayoutLedger(ledger CirclePayoutLedger) {
	circlePayoutLedgerInstance = ledger
}

func (self *CoreCircleApiClient) ListWalletTransactions(ctx context.Context, walletId string, from time.Time) ([]CircleTransaction, error) {
	return nil, errors.New("Circle wallet transaction listing not implemented")
}

func (self *CoreCircleApiClient) GetWalletTokenBalance(ctx context.Context, walletId string, tokenAddress string) (model.NanoCents, error) {
	return 0, errors.New("Circle wallet balance read not implemented")
}
