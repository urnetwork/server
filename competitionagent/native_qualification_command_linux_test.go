// All command tests inject synthetic identities/syscalls; the native adapter,
// child executable, Docker, and host resource limits never execute here.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"syscall"
	"testing"
)

// The actual encoder is exercised without converting its observation into an
// admission document, arbitrary command, or caller-provided pass flag.
func TestNativeQualificationCommandEncodesOnlyClosedChildFacts(t *testing.T) {
	system := &nativeQualificationCoreTestSystem{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}, setErrno: syscall.EPERM}
	var output bytes.Buffer
	if err := runNativeQualificationCommand(context.Background(), []string{"--probe-core"}, &output, defaultNativeQualificationCommandSettings(), system); err != nil {
		t.Fatal(err)
	}
	var facts nativeQualificationCoreFacts
	if decodeStrict(output.Bytes(), &facts) != nil || !validNativeQualificationCoreFacts(facts) {
		t.Fatal("actual command codec lost the concrete observation")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(output.Bytes(), &object) != nil {
		t.Fatal("observation JSON malformed")
	}
	for _, key := range []string{"accepted", "passed", "runtime_qualified", "source_approved"} {
		if _, present := object[key]; present {
			t.Fatal("child facts acquired class authority", key)
		}
	}
}

// Unknown/root-collection/argument-injection requests cannot reach a native
// operation while the complete root collector is still being implemented.
func TestNativeQualificationCommandRefusesUnknownAndPartialOperations(t *testing.T) {
	for _, args := range [][]string{nil, {"--collect"}, {"--probe-core", "--exec=synthetic"}, {"/bin/sh"}, {"--probe-unsupported"}} {
		system := &nativeQualificationCoreTestSystem{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}, setErrno: syscall.EPERM}
		var output bytes.Buffer
		if err := runNativeQualificationCommand(context.Background(), args, &output, defaultNativeQualificationCommandSettings(), system); !errors.Is(err, errNativeQualificationUnsupported) || len(system.calls) != 0 || output.Len() != 0 {
			t.Fatal("unsupported command dispatched or emitted success", args, err)
		}
	}
}

// Output/parent failures preserve their original cause and never turn a
// partial observation into a successful child operation.
func TestNativeQualificationCommandPreservesOutputAndParentBounds(t *testing.T) {
	system := &nativeQualificationCoreTestSystem{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}, setErrno: syscall.EPERM}
	settings := defaultNativeQualificationCommandSettings()
	settings.OutputBytes = 1
	var output bytes.Buffer
	if err := runNativeQualificationCommand(context.Background(), []string{"--probe-core"}, &output, settings, system); err == nil || output.Len() > 1 {
		t.Fatal("child output bound was bypassed", err, output.Len())
	}
	sentinel := errors.New("synthetic observation writer failed")
	if err := runNativeQualificationCommand(context.Background(), []string{"--probe-core"}, nativeQualificationCommandErrorWriter{err: sentinel}, defaultNativeQualificationCommandSettings(), system); !errors.Is(err, sentinel) {
		t.Fatal("original output writer failure was erased", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	untouched := &nativeQualificationCoreTestSystem{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}, setErrno: syscall.EPERM}
	output.Reset()
	if err := runNativeQualificationCommand(ctx, []string{"--probe-core"}, &output, defaultNativeQualificationCommandSettings(), untouched); !errors.Is(err, context.Canceled) || len(untouched.calls) != 0 || output.Len() != 0 {
		t.Fatal("canceled command dispatched kernel work", err)
	}
}

// Exercise the actual encoder's I/O error path without a host descriptor.
type nativeQualificationCommandErrorWriter struct{ err error }

// No bytes are acknowledged by this failed synthetic sink.
func (self nativeQualificationCommandErrorWriter) Write([]byte) (int, error) { return 0, self.err }
