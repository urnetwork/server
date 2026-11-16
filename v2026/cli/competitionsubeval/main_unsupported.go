//go:build !linux

// Unsupported entrypoints reject before reading authority or touching a host.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// The diagnostic is fixed and stdout never publishes a success or wire response.
func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "subevaluation executor requires Linux")
		os.Exit(1)
	}
}

// No role, configuration, input, command or context can supply Linux authority.
func run(_ context.Context, _ []string, _ io.Reader, _, _ io.Writer) error {
	return competitionagent.ErrUnsupportedPlatform
}
