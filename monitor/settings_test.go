package monitor

import (
	"slices"
	"testing"
	"time"
)

func TestSignalSettingsSSHKeyPathsBecomeIdentityArguments(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.SSHKeyPaths = []string{"/keys/edge", "/keys/db"}
	runner := newRunner(configFromSignalSettings(settings))
	args := runner.sshArgs("monitor@host", "true", 10*time.Second)
	for _, key := range settings.SSHKeyPaths {
		i := slices.Index(args, key)
		if i < 1 || args[i-1] != "-i" {
			t.Fatalf("ssh args do not contain -i %s: %v", key, args)
		}
	}
}
