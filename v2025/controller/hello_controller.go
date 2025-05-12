package controller

import (
	"github.com/urnetwork/server/v2025/session"
	// "github.com/urnetwork/server/v2025/model"
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
