package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/mcp"
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
	// logging entirely. Every way out of main flushes it first, so the last
	// lines written, a failure's report included, are not lost: the deferred
	// Flush for a return or a panic, and logs.Exit for an exit. See
	// server.ScrubCommandLogs.
	logs := server.ScrubCommandLogs()
	defer logs.Flush()

	// the flag package's own exit on a bad flag or -h would lose what it
	// wrote: it reports and the process exits as ExitOnError does, flushed
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			logs.Exit(0)
		}
		logs.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	if err := mcp.Run(ctx, mcp.RunOptions{Port: *port}); err != nil {
		panic(err)
	}
}
