package main

import (
	"context"
	"flag"
	"github.com/urnetwork/server/v2026"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server/v2026/mcp"
)

var (
	host = flag.String("host", "0.0.0.0", "host to listen on")
	port = flag.Int("port", 80, "port number to listen on")
	p    = flag.Int("p", 80, "port number to listen on (short form)")
)

func main() {
	// Address scrubbing for everything this process writes to stdout and
	// stderr, installed before anything can log. A failure here is not fatal:
	// it degrades to the previous unscrubbed behavior rather than losing
	// logging entirely. See server.ScrubProcessLogs.
	server.ScrubProcessLogs()

	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	if err := mcp.Run(ctx, mcp.RunOptions{Port: *port}); err != nil {
		panic(err)
	}
}
