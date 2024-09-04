package client

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"bringyour.com/connect"
)

var wvcLog = logFn("wallet_view_controller")

type AccountWallet struct {
	WalletId         *Id        `json:"wallet_id"`
	CircleWalletId   string     `json:"circle_wallet_id,omitempty"`
	NetworkId        *Id        `json:"network_id"`
	WalletType       WalletType `json:"wallet_type"`
	Blockchain       string     `json:"blockchain"`
	WalletAddress    string     `json:"wallet_address"`
	Active           bool       `json:"active"`
	DefaultTokenType string     `json:"default_token_type"`
	CreateTime       *Time      `json:"create_time"`
}

type WalletViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex
}

func newWalletViewController(ctx context.Context, device *BringYourDevice) *WalletViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &WalletViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,
	}
	return vc
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
func (vc *WalletViewController) AddExternalWallet(address string, blockchain Blockchain) (walletId *Id, err error) {

	blockchainUpper := strings.ToUpper(blockchain)
	if blockchainUpper != "SOL" && blockchainUpper != "MATIC" {
		return nil, fmt.Errorf("unsupported blockchain")
	}

	args := &CreateAccountWalletArgs{
		Blockchain:       blockchainUpper,
		WalletAddress:    address,
		DefaultTokenType: "USDC",
	}

	vc.device.Api().CreateAccountWallet(args, CreateAccountWalletCallback(connect.NewApiCallback[*CreateAccountWalletResult](
		func(result *CreateAccountWalletResult, createErr error) {

			if createErr != nil {
				err = createErr
				return
			}

			walletId = result.WalletId

		})))

	return

}

func (vc *WalletViewController) SetPayoutWallet(walletId *Id) (err error) {

	if walletId == nil {
		err = fmt.Errorf("no wallet id provided")
		return
	}

	args := &SetPayoutWalletArgs{
		WalletId: walletId,
	}

	vc.device.Api().SetPayoutWallet(args, SetPayoutWalletCallback(connect.NewApiCallback[*SetPayoutWalletResult](
		func(result *SetPayoutWalletResult, setWalletErr error) {

			if setWalletErr != nil {
				err = setWalletErr
			}

		})))

	return
}

func (vc *WalletViewController) GetPayoutWallet() (id *Id, err error) {

	vc.device.Api().GetPayoutWallet(GetPayoutWalletCallback(connect.NewApiCallback[*GetPayoutWalletIdResult](
		func(result *GetPayoutWalletIdResult, getWalletErr error) {

			if getWalletErr != nil {
				err = getWalletErr
			}

			id = result.Id

		})))

	return
}

func (vc *WalletViewController) GetAccountWallets() (accountWallets *AccountWalletsList, err error) {

	vc.device.Api().GetAccountWallets(connect.NewApiCallback[*GetAccountWalletsResult](
		func(results *GetAccountWalletsResult, walletsErr error) {

			if err != nil {
				err = walletsErr
				return
			}

			list := NewAccountWalletsList()

			for i := 0; i < results.Wallets.Len(); i++ {
				list.Add(results.Wallets.Get(i))
			}

			accountWallets = list

		}))

	return
}
