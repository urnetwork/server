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

type PaymentsListener interface {
	PaymentsChanged()
}

type IsCreatingExternalWalletListener interface {
	StateChanged(bool)
}

type IsRemovingWalletListener interface {
	StateChanged(bool)
}

type PayoutWalletListener interface {
	PayoutWalletChanged(*Id)
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

type AccountPayment struct {
	PaymentId       *Id       `json:"payment_id"`
	PaymentPlanId   *Id       `json:"payment_plan_id"`
	WalletId        *Id       `json:"wallet_id"`
	NetworkId       *Id       `json:"network_id"`
	PayoutByteCount ByteCount `json:"payout_byte_count"`
	Payout          NanoCents `json:"payout_nano_cents"`
	MinSweepTime    *Time     `json:"min_sweep_time"`
	CreateTime      *Time     `json:"create_time"`

	PaymentRecord  string  `json:"payment_record,omitempty"`
	TokenType      string  `json:"token_type"`
	TokenAmount    float64 `json:"token_amount,omitempty"`
	PaymentTime    *Time   `json:"payment_time,omitempty"`
	PaymentReceipt string  `json:"payment_receipt,omitempty"`

	Completed    bool  `json:"completed,omitempty"`
	CompleteTime *Time `json:"complete_time,omitempty"`

	Canceled   bool  `json:"canceled"`
	CancelTime *Time `json:"cancel_time,omitempty"`
}

type WalletViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	wallets                *AccountWalletsList
	isAddingExternalWallet bool
	isRemovingWallet       bool
	payoutWalletId         *Id
	accountPayments        *AccountPaymentsList

	stateLock sync.Mutex

	accountWalletsListeners           *connect.CallbackList[AccountWalletsListener]
	payoutWalletListeners             *connect.CallbackList[PayoutWalletListener]
	paymentsListeners                 *connect.CallbackList[PaymentsListener]
	isCreatingExternalWalletListeners *connect.CallbackList[IsCreatingExternalWalletListener]
	isRemovingWalletListeners         *connect.CallbackList[IsRemovingWalletListener]
}

func newWalletViewController(ctx context.Context, device *BringYourDevice) *WalletViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &WalletViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		wallets:                NewAccountWalletsList(),
		isAddingExternalWallet: false,
		isRemovingWallet:       false,
		payoutWalletId:         nil,
		accountPayments:        NewAccountPaymentsList(),

		accountWalletsListeners:           connect.NewCallbackList[AccountWalletsListener](),
		payoutWalletListeners:             connect.NewCallbackList[PayoutWalletListener](),
		paymentsListeners:                 connect.NewCallbackList[PaymentsListener](),
		isCreatingExternalWalletListeners: connect.NewCallbackList[IsCreatingExternalWalletListener](),
		isRemovingWalletListeners:         connect.NewCallbackList[IsRemovingWalletListener](),
	}
	return vc
}

func (vc *WalletViewController) Start() {
	vc.fetchAccountWallets()
	vc.FetchPayoutWallet()
	vc.fetchPayments()
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

	vc.device.GetApi().WalletValidateAddress(
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

func (vc *WalletViewController) AddIsCreatingExternalWalletListener(listener IsCreatingExternalWalletListener) Sub {
	callbackId := vc.isCreatingExternalWalletListeners.Add(listener)
	return newSub(func() {
		vc.accountWalletsListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) isCreatingExternalWalletChanged(isProcessing bool) {
	for _, listener := range vc.isCreatingExternalWalletListeners.Get() {
		connect.HandleError(func() {
			listener.StateChanged(isProcessing)
		})
	}
}

func (vc *WalletViewController) setIsCreatingExternalWallet(state bool) {

	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()

	vc.isAddingExternalWallet = state

	vc.isCreatingExternalWalletChanged(vc.isAddingExternalWallet)

}

func (vc *WalletViewController) AddExternalWallet(address string, blockchain Blockchain) {

	if !vc.isAddingExternalWallet {

		blockchainUpper := strings.ToUpper(blockchain)
		if blockchainUpper != "SOL" && blockchainUpper != "MATIC" {
			wvcLog("invalid blockchain passed: %s", blockchainUpper)
			return
		}

		vc.setIsCreatingExternalWallet(true)

		args := &CreateAccountWalletArgs{
			Blockchain:       blockchainUpper,
			WalletAddress:    address,
			DefaultTokenType: "USDC",
		}

		vc.device.GetApi().CreateAccountWallet(args, CreateAccountWalletCallback(connect.NewApiCallback[*CreateAccountWalletResult](
			func(result *CreateAccountWalletResult, err error) {

				if err != nil {
					wvcLog("error creating an external wallet: %s", err.Error())
					// err = createErr
					return
				}

				vc.setIsCreatingExternalWallet(false)
				vc.fetchAccountWallets()
				vc.FetchPayoutWallet()

			})))

	}

}

func (vc *WalletViewController) AddPayoutWalletListener(listener PayoutWalletListener) Sub {
	callbackId := vc.payoutWalletListeners.Add(listener)
	return newSub(func() {
		vc.payoutWalletListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) payoutWalletIdChanged(id *Id) {
	for _, listener := range vc.payoutWalletListeners.Get() {
		connect.HandleError(func() {
			listener.PayoutWalletChanged(id)
		})
	}
}

func (vc *WalletViewController) SetPayoutWallet(walletId *Id) (err error) {

	if walletId == nil {
		err = fmt.Errorf("no wallet id provided")
		return
	}

	args := &SetPayoutWalletArgs{
		WalletId: walletId,
	}

	vc.device.GetApi().SetPayoutWallet(args, SetPayoutWalletCallback(connect.NewApiCallback[*SetPayoutWalletResult](
		func(result *SetPayoutWalletResult, setWalletErr error) {

			if setWalletErr != nil {
				wvcLog("Error setting payout wallet: %s", err.Error())
				return
			}

			vc.stateLock.Lock()
			vc.payoutWalletId = args.WalletId
			vc.stateLock.Unlock()
			vc.payoutWalletIdChanged(vc.payoutWalletId)

		})))

	return
}

func (vc *WalletViewController) GetPayoutWalletId() (id *Id) {
	return vc.payoutWalletId
}

func (vc *WalletViewController) FetchPayoutWallet() {

	vc.device.GetApi().GetPayoutWallet(GetPayoutWalletCallback(connect.NewApiCallback[*GetPayoutWalletIdResult](
		func(result *GetPayoutWalletIdResult, err error) {

			if err != nil {
				wvcLog("error fetching payout wallet: %s", err.Error())
				return
			}

			vc.payoutWalletIdChanged(result.WalletId)

		})))
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

	vc.device.GetApi().GetAccountWallets(connect.NewApiCallback[*GetAccountWalletsResult](
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

			// we fetch wallets after removing a wallet
			if vc.isRemovingWallet {
				vc.setIsRemovingWallet(false)
			}

		}))

}

func (vc *WalletViewController) AddPaymentsListener(listener PaymentsListener) Sub {
	callbackId := vc.paymentsListeners.Add(listener)
	return newSub(func() {
		vc.paymentsListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) paymentsChanged() {
	for _, listener := range vc.paymentsListeners.Get() {
		connect.HandleError(func() {
			listener.PaymentsChanged()
		})
	}
}

func (vc *WalletViewController) GetAccountPayments() *AccountPaymentsList {
	return vc.accountPayments
}

func (vc *WalletViewController) setAccountPayments(payments []*AccountPayment) {

	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.accountPayments = NewAccountPaymentsList()
		vc.accountPayments.addAll(payments...)
	}()

	vc.paymentsChanged()

}

func (vc *WalletViewController) fetchPayments() {

	vc.device.GetApi().GetAccountPayments(connect.NewApiCallback[*GetNetworkAccountPaymentsResult](
		func(results *GetNetworkAccountPaymentsResult, err error) {

			if err != nil {
				return
			}

			payouts := []*AccountPayment{}

			for i := 0; i < results.AccountPayments.Len(); i += 1 {
				accountPayment := results.AccountPayments.Get(i)
				payouts = append(payouts, accountPayment)
			}

			vc.setAccountPayments(payouts)

		}))

}

func (vc *WalletViewController) AddIsRemovingWalletListener(listener IsRemovingWalletListener) Sub {
	callbackId := vc.isRemovingWalletListeners.Add(listener)
	return newSub(func() {
		vc.isRemovingWalletListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) isRemovingWalletChanged(isRemoving bool) {
	for _, listener := range vc.isRemovingWalletListeners.Get() {
		connect.HandleError(func() {
			listener.StateChanged(isRemoving)
		})
	}
}

func (vc *WalletViewController) setIsRemovingWallet(isRemoving bool) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()

		vc.isRemovingWallet = isRemoving
	}()

	vc.isRemovingWalletChanged(isRemoving)
}

func (vc *WalletViewController) RemoveWallet(walletId *Id) {

	if !vc.isRemovingWallet {
		vc.setIsRemovingWallet(true)

		vc.device.GetApi().RemoveWallet(
			&RemoveWalletArgs{
				WalletId: walletId.IdStr,
			},
			RemoveWalletCallback(connect.NewApiCallback[*RemoveWalletResult](
				func(result *RemoveWalletResult, err error) {

					if err != nil || !result.Success {
						vc.setIsRemovingWallet(false)
					}

					if result.Success {
						vc.fetchAccountWallets()
					}

				}),
			),
		)
	}

}
