// The qualification entrypoint has a closed command vocabulary and never
// loads server settings, production credentials, or an admission acceptance.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// A command instance owns its output ceiling; a peer cannot request unlimited
// output or supply an executable, shell command, or syscall number.
type nativeQualificationCommandSettings struct{ OutputBytes int64 }

// The fixed core observation is small; this is not a native evidence budget.
func defaultNativeQualificationCommandSettings() nativeQualificationCommandSettings {
	return nativeQualificationCommandSettings{OutputBytes: 8 * 1024}
}

// The new standalone CLI currently exposes only the concrete non-root child
// operation. Root collection dispatch is added only as its concrete handlers
// become complete; unknown/partial operations never execute a fallback.
func RunNativeQualification(ctx context.Context, args []string, output io.Writer) error {
	return runNativeQualificationCommand(ctx, args, output, defaultNativeQualificationCommandSettings(), nativeQualificationCoreNativeSystem{})
}

// Tests inject kernel interactions privately; the public entrypoint always
// uses the concrete syscall adapter above. A child observation is not a class.
func runNativeQualificationCommand(ctx context.Context, args []string, output io.Writer, settings nativeQualificationCommandSettings, system nativeQualificationCoreSystem) error {
	if len(args) != 1 || args[0] != "--probe-core" || output == nil || settings.OutputBytes <= 0 {
		return errNativeQualificationUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	facts, probeErr := captureNativeQualificationCoreFacts(ctx, system)
	writer := &boundedFileWriter{writer: output, maximum: settings.OutputBytes}
	encodeErr := json.NewEncoder(writer).Encode(facts)
	if writer.exceeded {
		encodeErr = errors.Join(encodeErr, errors.New("native child observation exceeded its output owner"))
	}
	if probeErr != nil || encodeErr != nil || ctx.Err() != nil {
		return errors.Join(probeErr, encodeErr, ctx.Err())
	}
	if !validNativeQualificationCoreFacts(facts) {
		return errors.New("native child did not observe exact core profile denials")
	}
	return nil
}
