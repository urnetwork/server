package main

import (
	"context"
	"fmt"

	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"bringyour.com/connect"
)

func main() {

	err := setupLocalUser()
	if err != nil {
		panic(err)
	}
}

func setupLocalUser() error {

	ctx := context.Background()

	userName := "test"
	userAuth := "test@bringyour.com"
	userPassword := "aaksdfkasd634"
	networkName := "thisisatest"

	client := connect.NewBringYourApi(
		ctx,
		connect.NewClientStrategyWithDefaults(ctx),
		"http://api:80",
	)

	var err error

	done := make(chan struct{})

	client.AuthLoginWithPassword(
		&connect.AuthLoginWithPasswordArgs{
			UserAuth: userAuth,
			Password: userPassword,
		},

		connect.NewApiCallback(func(result *connect.AuthLoginWithPasswordResult, e error) {
			defer close(done)
			if e != nil {
				err = fmt.Errorf("could not login: %w", e)
				return
			}

			if result.Error != nil {
				err = fmt.Errorf("could not login: %s", result.Error)
				return
			}

			fmt.Println("jwt: ", result.Network.ByJwt)
		}),
	)

	<-done

	if err == nil {
		fmt.Println("User logged in successfully")
		return nil
	}

	notJWTSession := session.NewLocalClientSession(ctx, "api:80", nil)

	result, err := model.NetworkCreate(model.NetworkCreateArgs{
		UserName:    userName,
		Password:    &userPassword,
		UserAuth:    &userAuth,
		NetworkName: networkName,
		Terms:       true,
	}, notJWTSession)

	if err != nil {
		return fmt.Errorf("could not create network: %w", err)
	}

	if result.Error != nil {
		return fmt.Errorf("could not create network: %s", result.Error)
	}

	createCodeResult, err := model.AuthVerifyCreateCode(
		model.AuthVerifyCreateCodeArgs{
			UserAuth: result.VerificationRequired.UserAuth,
		},
		notJWTSession,
	)

	if err != nil {
		return fmt.Errorf("could not create code: %w", err)
	}

	av, err := model.AuthVerify(
		model.AuthVerifyArgs{
			UserAuth:   result.VerificationRequired.UserAuth,
			VerifyCode: *createCodeResult.VerifyCode,
		},
		notJWTSession,
	)

	if err != nil {
		return fmt.Errorf("could not verify code: %w", err)
	}

	initialTransferBalance := model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024)

	balanceCodeA, err := model.CreateBalanceCode(
		ctx,
		initialTransferBalance,
		0,
		"test-1",
		"",
		"",
	)
	if err != nil {
		return fmt.Errorf("could not create balance code: %w", err)
	}

	byJwt, err := jwt.ParseByJwt(av.Network.ByJwt)
	if err != nil {
		return fmt.Errorf("could not parse jwt: %w", err)
	}

	_, err = model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret: balanceCodeA.Secret,
		},
		session.NewLocalClientSession(ctx, "0.0.0.0", byJwt),
	)
	if err != nil {
		return fmt.Errorf("could not redeem balance code: %w", err)
	}

	fmt.Println("Network created successfully")
	fmt.Println("jwt: ", av.Network.ByJwt)

	return nil
}
