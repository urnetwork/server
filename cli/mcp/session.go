package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urnetwork/server/session"
)

func createClientSession(ctx context.Context, req *mcp.CallToolRequest) (*session.ClientSession, error) {

	// todo - if request has an API key, get associated JWT and create session with that

	// for now, unauthenticated local session
	return session.NewLocalClientSession(ctx, "0.0.0.0:80", nil), nil

}

// func createClientSession(ctx context.Context, req *mcp.CallToolRequest) (*session.ClientSession, error) {

// 	// todo - if request has an API key, get associated JWT and create session with that

// 	// Extract client address from headers (MCP SDK provides headers via Extra.Header)
// 	clientAddress := ""

// 	if req.Extra != nil && req.Extra.Header != nil {
// 		clientAddress = req.Extra.Header.Get("X-UR-Forwarded-For")

// 		if clientAddress == "" {
// 			clientAddress = req.Extra.Header.Get("X-Forwarded-For")
// 		}

// 		if clientAddress == "" {
// 			clientAddress = req.Extra.Header.Get("X-Real-IP")
// 		}
// 	}

// 	// Fallback for testing/local development
// 	if clientAddress == "" {
// 		clientAddress = "127.0.0.1:0"
// 	}

// 	cancelCtx, cancel := context.WithCancel(ctx)

// 	return &session.ClientSession{
// 		Ctx:           cancelCtx,
// 		Cancel:        cancel,
// 		ClientAddress: clientAddress,
// 		Header:        map[string][]string(req.Extra.Header),
// 	}, nil
// }
