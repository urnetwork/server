package main

import (
	"context"
	"flag"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server/mcp"
)

var (
	host = flag.String("host", "0.0.0.0", "host to listen on")
	port = flag.Int("port", 80, "port number to listen on")
	p    = flag.Int("p", 80, "port number to listen on (short form)")
)

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	if err := mcp.Run(ctx, mcp.RunOptions{Port: *port}); err != nil {
		panic(err)
	}
}
