package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urnetwork/server/session"
)

func createClientSession(ctx context.Context, req *mcp.CallToolRequest) (*session.ClientSession, error) {

	// todo - if request has an API key, get associated JWT and create session with that

	// for now, unauthenticated local session
	return session.NewLocalClientSession(ctx, "1.1.1.1:0", nil), nil

}
