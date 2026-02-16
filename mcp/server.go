package main

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

func runServer(ctx context.Context, port int) error {
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "urnetwork-mcp",
		Version: "0.0.1-beta.1",
	}, nil)

	// add logging
	mcpServer.AddReceivingMiddleware(createLoggingMiddleware())

	// register tools
	registerTools(mcpServer)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return mcpServer
	}, nil)

	// Wrap MCP handler to match http.HandlerFunc signature
	mcpHandlerFunc := func(w http.ResponseWriter, r *http.Request) {
		mcpHandler.ServeHTTP(w, r)
	}

	// Define routes using the router package
	routes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("*", ".*", mcpHandlerFunc),
	}

	listenIpv4, _, listenPort := server.RequireListenIpPort(port)

	reusePort := false

	httpServerOptions := server.HttpServerOptions{
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  5 * time.Minute,
	}

	glog.Infof("MCP server listening on %s:%d", listenIpv4, listenPort)

	return server.HttpListenAndServeWithReusePort(
		ctx,
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
		router.NewRouter(ctx, routes),
		reusePort,
		httpServerOptions,
	)
}
