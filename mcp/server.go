package main

import (
	"log"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func runServer(addr string) error {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "urnetwork-mcp",
		Version: "0.0.1-beta.1",
	}, nil)

	// add logging
	server.AddReceivingMiddleware(createLoggingMiddleware())

	// register tools
	registerTools(server)

	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)

	log.Printf("MCP server listening on %s", addr)

	return http.ListenAndServe(addr, handler)
}
