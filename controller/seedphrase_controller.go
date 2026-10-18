package controller

import (
	"errors"

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

// Test-only scheduling hook between GenerateSeedphrase's existence check and
// its create. Production leaves it nil; tests use it to create the seedphrase
// auth in between, as a concurrent generate would, without scheduler luck.
var generateSeedphraseAfterCheck func()

func seedphraseExistsResult() *GenerateSeedphraseResult {
	return &GenerateSeedphraseResult{
		Error: &GenerateSeedphraseError{
			Message: "Seedphrase already exists, use regenerate instead.",
		},
	}
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
		return seedphraseExistsResult(), nil
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

	if generateSeedphraseAfterCheck != nil {
		generateSeedphraseAfterCheck()
	}
	seedphrase, err := model.GenerateSeedphrase(session.Ctx, session.ByJwt.UserId)
	if errors.Is(err, model.ErrSeedphraseAuthExists) {
		// a concurrent generate won: the same answer as the check above
		return seedphraseExistsResult(), nil
	}
	if err != nil {
		return nil, err
	}

	model.RecordAccountActionAttempt(session.Ctx, session.ByJwt.UserId, model.AccountActionGenerateSeedphrase)

	return &GenerateSeedphraseResult{
		Seedphrase: seedphrase,
	}, nil
}
