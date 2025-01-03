package connprovider

import (
	"context"
	"errors"
	"fmt"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
)

func LoginWithCredentials(ctx context.Context, apiURL, userAuth, password string) (string, error) {
	api := connect.NewBringYourApi(
		ctx,
		connect.NewClientStrategyWithDefaults(ctx),
		apiURL,
	)

	// api.AuthNetworkClient()
	type loginResult struct {
		res *connect.AuthLoginWithPasswordResult
		err error
	}

	resChan := make(chan loginResult)

	api.AuthLoginWithPassword(
		&connect.AuthLoginWithPasswordArgs{
			UserAuth: userAuth,
			Password: password,
		},
		connect.NewApiCallback(
			func(res *connect.AuthLoginWithPasswordResult, err error) {
				resChan <- loginResult{res, err}
			},
		),
	)

	res := <-resChan
	if res.res.Error != nil {
		return "", errors.New(res.res.Error.Message)
	}

	if res.res.VerificationRequired != nil {
		return "", errors.New("verification required")
	}

	return res.res.Network.ByJwt, nil

}

func authNetworkClient(ctx context.Context, apiURL, jwt string, req *connect.AuthNetworkClientArgs) (string, error) {
	strategy := connect.NewClientStrategyWithDefaults(ctx)
	api := connect.NewBringYourApi(ctx, strategy, apiURL)
	api.SetByJwt(jwt)

	res, err := api.AuthNetworkClientSync(req)
	if err != nil {
		return "", fmt.Errorf("auth network client failed: %w", err)
	}

	if res.Error != nil {
		return "", errors.New(res.Error.Message)
	}

	return res.ByClientJwt, nil
}

func parseByJwtClientId(byJwt string) (connect.Id, error) {
	claims := gojwt.MapClaims{}
	gojwt.NewParser().ParseUnverified(byJwt, claims)

	jwtClientId, ok := claims["client_id"]
	if !ok {
		return connect.Id{}, fmt.Errorf("byJwt does not contain claim client_id")
	}
	switch v := jwtClientId.(type) {
	case string:
		return connect.ParseId(v)
	default:
		return connect.Id{}, fmt.Errorf("byJwt hav invalid type for client_id: %T", v)
	}
}
