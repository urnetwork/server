package controller

// The versioned route owns only ordinary client registration. It does not
// replay payment/proxy mutations when a committed response is lost.

import (
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Onboarding remains an optional idempotent projection after atomic identity
// allocation or recovery; it cannot change the registration's bound identity.
func RegisterNetworkClient(args *model.RegisterNetworkClientArgs, clientSession *session.ClientSession) (*model.RegisterNetworkClientResult, error) {
	result, err := model.RegisterNetworkClient(args, clientSession)
	if err == nil && result != nil && result.Error == nil && result.ClientId != nil {
		recordAuthNetworkClientOnboarding(&model.AuthNetworkClientArgs{Description: args.Description, DeviceSpec: args.DeviceSpec}, clientSession)
	}
	return result, err
}
