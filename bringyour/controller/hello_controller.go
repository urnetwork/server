package controller

import (
	"github.com/urnetwork/server/bringyour/session"
	// "github.com/urnetwork/server/bringyour/model"
)

type HelloResult struct {
	ClientAddress string `json:"client_address,omitempty"`
}

func Hello(
	session *session.ClientSession,
) (*HelloResult, error) {
	result := &HelloResult{
		ClientAddress: session.ClientAddress,
	}
	return result, nil
}
