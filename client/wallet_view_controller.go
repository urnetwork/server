package client

import (
	"context"
	"sync"
	"time"
)

var wvcLog = logFn("wallet_view_model")

type WalletViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex
}

func newWalletViewController(ctx context.Context, device *BringYourDevice) *WalletViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vm := &WalletViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,
	}
	return vm
}

func (vc *WalletViewController) Start() {
}

func (vc *WalletViewController) Stop() {
	// FIXME
}

func (vc *WalletViewController) Close() {
	wvcLog("close")

	vc.cancel()
}

func (vc *WalletViewController) GetNextPayoutDate() string {
	now := time.Now().UTC()
	year := now.Year()
	month := now.Month()
	day := now.Day()

	var nextPayoutDate time.Time

	switch {
	case day < 15:
		nextPayoutDate = time.Date(year, month, 15, 0, 0, 0, 0, time.UTC)
	case month == time.December:
		nextPayoutDate = time.Date(year+1, time.January, 1, 0, 0, 0, 0, time.UTC)
	default:
		nextPayoutDate = time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
	}

	return nextPayoutDate.Format("Jan 2")
}
