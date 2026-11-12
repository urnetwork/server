// What the commands that reach an env through server's packages share. Such a command runs as the env's own
// processes do, under the WARP_ENV that names the env, with its config and vault where server's resolver finds them
// (server/env.go), and so under server's dial guards.

package main

import (
	"errors"
	"fmt"

	"github.com/urnetwork/server"
)

// The env this process runs as, or an error when WARP_ENV names none: without one server's resolver has no env to
// read, and a command must not guess which one the operator meant.
func sidecarctlProcessEnv() (string, error) {
	env, err := server.Env()
	if err != nil {
		return "", errors.New("WARP_ENV is not set, and this reaches the env it names; run it as that env's services run")
	}
	return env, nil
}

// Runs a call into server's packages, which raise where a command returns: a raise comes back as the error, so the
// command exits 1 with it and never with a stack.
func sidecarctlServerCall(callback func() error) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%v", recovered)
			}
		}
	}()
	return callback()
}
