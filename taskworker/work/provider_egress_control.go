package work

import (
	"context"
	"encoding/base64"
	"errors"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"google.golang.org/protobuf/proto"
)

// One pass-owned authority shares the pass's notification owner. Per-request
// calls do not create publishers. The caller closes notifications after every
// private tunnel/OOB owner has joined.
type providerEgressControl struct {
	credentials   *providerEgressCredentials
	notifications *model.ContractOriginNotifications
}

func newProviderEgressControl(credentials *providerEgressCredentials, notifications *model.ContractOriginNotifications) (*providerEgressControl, error) {
	if credentials == nil || notifications == nil {
		return nil, errors.New("provider control requires credential and notification owners")
	}
	return &providerEgressControl{credentials: credentials, notifications: notifications}, nil
}

func (self *providerEgressControl) ConnectControl(ctx context.Context, token string, args *connect.ConnectControlArgs) (*connect.ConnectControlResult, error) {
	ctx = session.WithStateQuerySource(ctx, session.StateQueryProberControl)
	return server.HandleError2(func() (*connect.ConnectControlResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if args == nil {
			return nil, errors.New("provider control request is absent")
		}
		claims, err := self.credentials.parse(ctx, token, session.ByJwtAudienceApi)
		if err != nil {
			return nil, err
		}
		if claims == nil || claims.NetworkId != self.credentials.networkId || claims.UserId != self.credentials.userId || claims.ClientId == nil || claims.DeviceId == nil || *claims.ClientId == self.credentials.clientId {
			return nil, errors.New("provider control credential is outside its owner")
		}
		if _, ok := self.credentials.children.Load(*claims.ClientId); !ok {
			return nil, errors.New("provider control client was not minted by its owner")
		}
		// Membership in the owner is not current authorization. Recheck signature,
		// audience, revocation and credential rotation on every request, cleanup included.
		if err := self.credentials.validate(ctx, claims, true); err != nil {
			return nil, err
		}
		packBytes, err := connect.DecodeBase64(base64.StdEncoding, args.Pack)
		if err != nil {
			return nil, err
		}
		defer connect.MessagePoolReturn(packBytes)
		pack := &protocol.Pack{}
		if err := proto.Unmarshal(packBytes, pack); err != nil {
			return nil, err
		}
		frames, controlErr := controller.ConnectControlFrames(model.WithContractOriginNotifications(ctx, self.notifications), *claims.ClientId, pack.Frames, connect.DefaultContractManagerSettings())
		defer func() {
			for _, frame := range frames {
				connect.MessagePoolReturn(frame.MessageBytes)
			}
		}()
		bytes, err := proto.Marshal(&protocol.Pack{Frames: frames})
		if err != nil {
			return nil, err
		}
		result := &connect.ConnectControlResult{Pack: connect.EncodeBase64(base64.StdEncoding, bytes)}
		if controlErr != nil {
			result.Error = &connect.ConnectControlError{Message: controlErr.Error()}
		}
		return result, nil
	}, func(err error) (*connect.ConnectControlResult, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, err
	})
}

// The pass owns one publisher independently of cancellation. run must join all
// tunnel/OOB owners, including bounded late cleanup, before returning. This
// boundary then stops and joins the publisher; it never detaches a background
// owner beyond the pass invocation.
func runWithProviderEgressControl(ctx context.Context, credentials *providerEgressCredentials,
	run func(connect.NetworkClientControl) (*ProviderEgressProbeResult, error),
) (*ProviderEgressProbeResult, error) {
	notificationCtx, cancelNotifications := context.WithCancel(context.WithoutCancel(ctx))
	notifications := model.NewContractOriginNotifications(notificationCtx, model.DefaultContractOriginNotificationSettings())
	defer func() { cancelNotifications(); notifications.Close() }()
	control, err := newProviderEgressControl(credentials, notifications)
	if err != nil {
		return nil, err
	}
	return run(control)
}
