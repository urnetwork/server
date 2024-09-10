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

type AccountWalletsListener interface {
	AccountWalletsChanged()
}

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

type Payout struct {
	WalletId        *Id     `json:"wallet_id"`
	WalletAddress   string  `json:"wallet_address"`
	CompleteTimeFmt string  `json:"complete_time"` // formatted to "Jan 2"
	AmountUsd       float32 `json:"amount_usd"`
}

type WalletViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	wallets *AccountWalletsList

	stateLock sync.Mutex

	accountWalletsListeners *connect.CallbackList[AccountWalletsListener]
}

func newWalletViewController(ctx context.Context, device *BringYourDevice) *WalletViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &WalletViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		wallets: NewAccountWalletsList(),

		accountWalletsListeners: connect.NewCallbackList[AccountWalletsListener](),
	}
	return vc
}

func (vc *WalletViewController) Start() {
	vc.fetchAccountWallets()
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

type ValidateAddressCallback interface {
	SendResult(valid bool)
}

type validateAddressCallbackImpl struct {
	sendResult func(bool)
}

func (v *validateAddressCallbackImpl) SendResult(valid bool) {
	v.sendResult(valid)
}

// Return the concrete type instead of the interface pointer
func NewValidateAddressCallback(sendResult func(bool)) *validateAddressCallbackImpl {
	return &validateAddressCallbackImpl{sendResult: sendResult}
}

func (vc *WalletViewController) ValidateAddress(
	address string,
	blockchain Blockchain,
	callback ValidateAddressCallback,
) {

	vc.device.Api().WalletValidateAddress(
		&WalletValidateAddressArgs{
			Address: address,
			Chain:   blockchain,
		},
		connect.NewApiCallback[*WalletValidateAddressResult](
			func(result *WalletValidateAddressResult, err error) {

				if err != nil {
					wvcLog("error validating address %s on %s: %s", address, blockchain, err.Error())
					callback.SendResult(false)
				}

				callback.SendResult(result.Valid)
			}),
	)

}

func (vc *WalletViewController) AddExternalWallet(address string, blockchain Blockchain) error {

	blockchainUpper := strings.ToUpper(blockchain)
	if blockchainUpper != "SOL" && blockchainUpper != "MATIC" {
		return fmt.Errorf("unsupported blockchain")
	}

	args := &CreateAccountWalletArgs{
		Blockchain:       blockchainUpper,
		WalletAddress:    address,
		DefaultTokenType: "USDC",
	}

	vc.device.Api().CreateAccountWallet(args, CreateAccountWalletCallback(connect.NewApiCallback[*CreateAccountWalletResult](
		func(result *CreateAccountWalletResult, createErr error) {

			if createErr != nil {
				// err = createErr
				return
			}

			// walletId = result.WalletId

			vc.accountWalletsChanged()

		})))

	return nil

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

func (vc *WalletViewController) GetWallets() *AccountWalletsList {
	return vc.wallets
}

func (vc *WalletViewController) FilterWalletsById(idStr string) *AccountWallet {

	id, err := ParseId(idStr)
	if err != nil {
		return nil
	}

	for i := 0; i < vc.wallets.Len(); i++ {

		wallet := vc.wallets.Get(i)

		if wallet.WalletId.Cmp(id) == 0 {
			return wallet
		}

	}

	return nil

}

func (vc *WalletViewController) AddAccountWalletsListener(listener AccountWalletsListener) Sub {
	callbackId := vc.accountWalletsListeners.Add(listener)
	return newSub(func() {
		vc.accountWalletsListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) accountWalletsChanged() {
	for _, listener := range vc.accountWalletsListeners.Get() {
		connect.HandleError(func() {
			listener.AccountWalletsChanged()
		})
	}
}

func (vc *WalletViewController) fetchAccountWallets() {

	vc.device.Api().GetAccountWallets(connect.NewApiCallback[*GetAccountWalletsResult](
		func(results *GetAccountWalletsResult, err error) {

			if err != nil {
				wvcLog("Error fetching account wallets: ", err.Error())
				return
			}

			newWalletsList := NewAccountWalletsList()
			var wallets []*AccountWallet

			for i := 0; i < results.Wallets.Len(); i++ {

				walletResult := results.Wallets.Get(i)

				wallet := &AccountWallet{
					WalletId:         walletResult.WalletId,
					CircleWalletId:   walletResult.CircleWalletId,
					NetworkId:        walletResult.NetworkId,
					WalletType:       walletResult.WalletType,
					Blockchain:       walletResult.Blockchain,
					WalletAddress:    walletResult.WalletAddress,
					Active:           walletResult.Active,
					DefaultTokenType: walletResult.DefaultTokenType,
					CreateTime:       walletResult.CreateTime,
				}

				wallets = append(wallets, wallet)

			}

			newWalletsList.addAll(wallets...)

			vc.wallets = newWalletsList

			vc.accountWalletsChanged()

		}))

}
