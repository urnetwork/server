// The existing-only role is a closed entrypoint, never another ordinary mode.
package main

import (
	"io"
	"testing"
)

func TestExistingArgumentsCannotCombineFreshOrExecutionRoles(t *testing.T) {
	for _, role := range []string{"--serve-existing", "--serve-existing-managed"} {
		parsed, err := parseArguments([]string{"--config", "/synthetic/protected-observer.json", role}, io.Discard)
		if err != nil || parsed.serve || parsed.serveManaged || parsed.executeRunId != "" || parsed.dockerOwnerRunId != "" || parsed.recoverActive {
			t.Fatal("closed observer role changed ordinary role selection", err)
		}
		for _, extra := range []string{"--serve", "--serve-managed", "--execute=synthetic-run", "--docker-owner=synthetic-run", "--recover-active", "--start-private-engine", "--docker-probe"} {
			if _, err := parseArguments([]string{"--config", "/synthetic/protected-observer.json", role, extra}, io.Discard); err == nil {
				t.Fatal("observer combined with another privileged role", role, extra)
			}
		}
		if _, err := parseArguments([]string{"--config", "/synthetic/protected-observer.json", role, "arbitrary-command"}, io.Discard); err == nil {
			t.Fatal("observer accepted caller-selected command arguments")
		}
	}
}
