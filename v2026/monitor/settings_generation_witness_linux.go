// Linux binds the writer to its actual unit/process and mapped executable.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// Reuse the bounded local process/cgroup/unit authority; do not accept an argv
// or environment claim as proof of the writer's native owner. This runs once.
func settingsWitnessSelfIdentity(ctx context.Context, options SettingsGenerationWitnessSettings) (settingsWitnessIdentity, error) {
	owner, err := sshAdmissionCurrentOwner(ctx)
	if err != nil || ctx.Err() != nil || owner.Identity.Pid != os.Getpid() {
		return settingsWitnessIdentity{}, errors.New("settings witness owner unavailable")
	}
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		return settingsWitnessIdentity{}, errors.New("settings witness executable unavailable")
	}
	defer executable.Close()
	before, err := executable.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > options.MaxExecutableBytes {
		return settingsWitnessIdentity{}, errors.New("settings witness executable unavailable")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(executable, before.Size()+1))
	after, statErr := executable.Stat()
	if err != nil || statErr != nil || written != before.Size() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || ctx.Err() != nil {
		return settingsWitnessIdentity{}, errors.New("settings witness executable changed")
	}
	confirmedOwner, err := sshAdmissionCurrentOwner(ctx)
	if err != nil || confirmedOwner != owner || ctx.Err() != nil {
		return settingsWitnessIdentity{}, errors.New("settings witness owner changed")
	}
	identity := settingsWitnessIdentity{
		Pid: owner.Identity.Pid, StartTicks: owner.Identity.StartTicks, BootId: owner.Identity.BootId,
		InvocationId: owner.InvocationId, ExecutableSha256: hex.EncodeToString(digest.Sum(nil)),
	}
	if !identity.valid() {
		return settingsWitnessIdentity{}, errors.New("settings witness identity invalid")
	}
	return identity, nil
}
