package main

import (
	"context"
	"fmt"

	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"github.com/jedib0t/go-pretty/v6/progress"
)

func setupNewNetwork(ctx context.Context, pw progress.Writer) (clientJWT string, err error) {

	tracker := &progress.Tracker{
		Message: "Setup new network",
		Total:   5,
		// Units:   *units,
	}

	pw.AppendTracker(tracker)
	tracker.Start()

	defer func() {
		if err != nil {
			tracker.UpdateMessage(fmt.Sprintf("Setup new network failed: %v", err))
			tracker.MarkAsErrored()
			return
		}
		tracker.UpdateMessage("Setup new network is ready")
		tracker.MarkAsDone()
	}()

	notJWTSession := session.NewLocalClientSession(ctx, "localhost:1234", nil)

	userName := "test"
	userAuth := "test@bringyour.com"
	userPassword := "aaksdfkasd634"
	networkName := "thisisatest"

	result, err := model.NetworkCreate(model.NetworkCreateArgs{
		UserName:    userName,
		Password:    &userPassword,
		UserAuth:    &userAuth,
		NetworkName: networkName,
		Terms:       true,
	}, notJWTSession)

	if err != nil {
		return "", fmt.Errorf("failed to create network: %w", err)
	}

	if result.Error != nil {
		return "", fmt.Errorf("failed to create network: %s", *result.Error)
	}

	tracker.Increment(1)

	if result.VerificationRequired == nil {
		return "", fmt.Errorf("verification was unexpectedly not required")
	}

	createCodeResult, err := model.AuthVerifyCreateCode(
		model.AuthVerifyCreateCodeArgs{
			UserAuth: result.VerificationRequired.UserAuth,
		},
		notJWTSession,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create verification code: %w", err)
	}

	if createCodeResult.Error != nil {
		return "", fmt.Errorf("failed to create verification code: %s", *createCodeResult.Error)
	}

	tracker.Increment(1)

	av, err := model.AuthVerify(
		model.AuthVerifyArgs{
			UserAuth:   result.VerificationRequired.UserAuth,
			VerifyCode: *createCodeResult.VerifyCode,
		},
		notJWTSession,
	)

	if err != nil {
		return "", fmt.Errorf("failed to verify: %w", err)
	}

	if av.Error != nil {
		return "", fmt.Errorf("failed to verify: %s", *av.Error)
	}

	tracker.Increment(1)

	login, err := model.AuthLoginWithPassword(model.AuthLoginWithPasswordArgs{
		UserAuth: userAuth,
		Password: userPassword,
	}, notJWTSession)

	if err != nil {
		return "", fmt.Errorf("failed to login: %w", err)
	}

	byJwt, err := jwt.ParseByJwt(*login.Network.ByJwt)
	if err != nil {
		return "", fmt.Errorf("failed to parse by jwt: %w", err)
	}

	jwtSession := session.NewLocalClientSession(ctx, "localhost:1234", byJwt)

	cl, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{
		Description: "test",
		DeviceSpec:  "test",
	}, jwtSession)
	if err != nil {
		return "", fmt.Errorf("failed to create network client jwt: %w", err)
	}

	tracker.Increment(1)

	return *cl.ByClientJwt, nil

}
