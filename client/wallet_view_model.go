package client

import (
	"context"
	"sync"
	"time"
)

var wvmLog = logFn("wallet_view_model")

type WalletViewModel struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex
}

func newWalletViewModel(ctx context.Context, device *BringYourDevice) *WalletViewModel {
	cancelCtx, cancel := context.WithCancel(ctx)

	vm := &WalletViewModel{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,
	}
	return vm
}

func (vm *WalletViewModel) Start() {
}

func (vm *WalletViewModel) Stop() {
	// FIXME
}

func (vm *WalletViewModel) Close() {
	wvmLog("close")

	vm.cancel()
}

func (vm *WalletViewModel) GetNextPayoutDate() string {
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
