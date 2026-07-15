package controller

import (
	"fmt"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type RegenerateSeedphraseArgs struct {
}

type RegenerateSeedphraseResult struct {
	Seedphrase string `json:"seedphrase"`
}

type GenerateSeedphraseArgs struct {
}

type GenerateSeedphraseResult struct {
	Seedphrase string `json:"seedphrase"`
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
		return nil, fmt.Errorf("No seedphrase auth found.")
	}

	seedphrase, err := model.RegenerateSeedphrase(session.Ctx, session.ByJwt.UserId)
	if err != nil {
		return nil, err
	}

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
		return nil, fmt.Errorf("Seedphrase auth already exists.")
	}

	seedphrase, err := model.GenerateSeedphrase(session.Ctx, session.ByJwt.UserId)
	if err != nil {
		return nil, err
	}

	return &GenerateSeedphraseResult{
		Seedphrase: seedphrase,
	}, nil
}
