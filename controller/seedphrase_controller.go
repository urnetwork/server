package controller

import (
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type RegenerateSeedphraseArgs struct {
}

type RegenerateSeedphraseResult struct {
	Seedphrase string                     `json:"seedphrase"`
	Error      *RegenerateSeedphraseError `json:"error,omitempty"`
}

type RegenerateSeedphraseError struct {
	Message string `json:"message"`
}

type GenerateSeedphraseArgs struct {
}

type GenerateSeedphraseResult struct {
	Seedphrase string                   `json:"seedphrase"`
	Error      *GenerateSeedphraseError `json:"error,omitempty"`
}

type GenerateSeedphraseError struct {
	Message string `json:"message"`
}

func RegenerateSeedphrase(
	args RegenerateSeedphraseArgs,
	session *session.ClientSession,
) (*RegenerateSeedphraseResult, error) {
	hasSeedphrase, err := model.HasSeedphraseAuth(session.Ctx, session.ByJwt.UserId)
	if err != nil {
		return nil, err
	}
	if !hasSeedphrase {
		return &RegenerateSeedphraseResult{
			Error: &RegenerateSeedphraseError{
				Message: "No seedphrase auth found.",
			},
		}, nil
	}

	if err := model.CheckAccountActionRateLimit(
		session.Ctx,
		session.ByJwt.UserId,
		model.AccountActionRegenerateSeedphrase,
		model.AccountActionRegenerateSeedphraseDailyLimit,
		model.AccountActionDailyWindow,
	); err != nil {
		return &RegenerateSeedphraseResult{
			Error: &RegenerateSeedphraseError{
				Message: err.Error(),
			},
		}, nil
	}

	seedphrase, err := model.RegenerateSeedphrase(session.Ctx, session.ByJwt.UserId)
	if err != nil {
		return nil, err
	}

	model.RecordAccountActionAttempt(session.Ctx, session.ByJwt.UserId, model.AccountActionRegenerateSeedphrase)

	return &RegenerateSeedphraseResult{
		Seedphrase: seedphrase,
	}, nil
}

func GenerateSeedphrase(
	args GenerateSeedphraseArgs,
	session *session.ClientSession,
) (*GenerateSeedphraseResult, error) {
	hasSeedphrase, err := model.HasSeedphraseAuth(session.Ctx, session.ByJwt.UserId)
	if err != nil {
		return nil, err
	}
	if hasSeedphrase {
		return &GenerateSeedphraseResult{
			Error: &GenerateSeedphraseError{
				Message: "Seedphrase already exists, use regenerate instead.",
			},
		}, nil
	}

	if err := model.CheckAccountActionRateLimit(
		session.Ctx,
		session.ByJwt.UserId,
		model.AccountActionGenerateSeedphrase,
		model.AccountActionGenerateSeedphraseDailyLimit,
		model.AccountActionDailyWindow,
	); err != nil {
		return &GenerateSeedphraseResult{
			Error: &GenerateSeedphraseError{
				Message: err.Error(),
			},
		}, nil
	}

	seedphrase, err := model.GenerateSeedphrase(session.Ctx, session.ByJwt.UserId)
	if err != nil {
		return nil, err
	}

	model.RecordAccountActionAttempt(session.Ctx, session.ByJwt.UserId, model.AccountActionGenerateSeedphrase)

	return &GenerateSeedphraseResult{
		Seedphrase: seedphrase,
	}, nil
}
