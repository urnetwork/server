package main

import (
	"context"
	"errors"
	"io"
	"strconv"
	"time"
)

const (
	sshLeaseDefaultLifetime = 240 * time.Second
	sshLeaseMaximumLifetime = 1900 * time.Second
)

// The optional lifetime belongs to this helper, including queue admission.
// Its separately qualified owner must fit setup, work and joined cleanup
// inside both this lifetime and the owner's existing service deadline.
// A helper timeout still retains its active reservation until native cleanup.
func parseSshLeaseInvocation(args []string) (string, time.Duration, error) {
	if (len(args) != 2 && len(args) != 4) || args[0] != "ssh-lease" || args[1] == "" {
		return "", 0, errors.New("usage: monitor ssh-lease PRIVATE_REQUEST_JSON [--lifetime-seconds N]")
	}
	lifetime := sshLeaseDefaultLifetime
	if len(args) == 4 {
		seconds, err := strconv.Atoi(args[3])
		if args[2] != "--lifetime-seconds" || err != nil || strconv.Itoa(seconds) != args[3] || seconds < 1 || seconds > int(sshLeaseMaximumLifetime/time.Second) {
			return "", 0, errors.New("SSH lease lifetime must be an integer from 1 through 1900 seconds")
		}
		lifetime = time.Duration(seconds) * time.Second
	}
	return args[1], lifetime, nil
}

func runSshLeaseCommand(parent context.Context, args []string, input io.Reader, output io.Writer, runLease func(context.Context, string, io.Reader, io.Writer) error) error {
	request, lifetime, err := parseSshLeaseInvocation(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, lifetime)
	defer cancel()
	return runLease(ctx, request, input, output)
}
