package client

import (
	"context"

	"bringyour.com/connect"
)

var referralCodeLog = logFn("referral_code_view_controller")

type ReferralCodeViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device *BringYourDevice
}

func newReferralCodeViewController(ctx context.Context, device *BringYourDevice) *ReferralCodeViewController {

	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ReferralCodeViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,
	}
	return vc
}

func (vc *ReferralCodeViewController) Start() {}

func (vc *ReferralCodeViewController) Stop() {}

func (vc *ReferralCodeViewController) Close() {
	referralCodeLog("close")

	vc.cancel()
}

func (vc *ReferralCodeViewController) GetNetworkReferralCode() (code string, err error) {
	vc.device.Api().GetNetworkReferralCode(
		GetNetworkReferralCodeCallback(
			connect.NewApiCallback[*GetNetworkReferralCodeResult](
				func(result *GetNetworkReferralCodeResult, apiErr error) {
					if apiErr != nil {
						err = apiErr
						return
					}

					code = result.ReferralCode

				},
			),
		),
	)

	return
}
