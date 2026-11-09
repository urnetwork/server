// Namespace handles are valid foreign mount roots, never candidate quota proof.
package competitionagent

import (
	"bytes"
	"context"
	"testing"
)

// Exercise full admission with Docker-style namespace mounts outside the quota.
func TestRuntimeResourceBoundaryAcceptsForeignNamespaceHandleMounts(t *testing.T) {
	for _, kind := range []string{"net", "mnt", "pid", "user", "uts", "ipc", "cgroup", "time"} {
		fixture := newRuntimeBoundaryTest(t)
		line := []byte("3 1 0:42 " + kind + ":[123456789] /run/synthetic-runtime/netns/handle rw - nsfs nsfs rw\n")
		fixture.host.facts.MountInfo = append(fixture.host.facts.MountInfo, line...)
		fixture.host.facts.MountInfoAfter = bytes.Clone(fixture.host.facts.MountInfo)
		proof, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host)
		if err != nil {
			t.Fatalf("valid foreign %s namespace mount rejected: %v", kind, err)
		}
		if err := proof.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A valid handle mounted inside writable roots must still fail the quota gate.
func TestRuntimeResourceBoundaryRejectsNestedNamespaceHandleMount(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	line := []byte("3 2 0:42 net:[123456789] " + fixture.boundary.WorkRoot + " rw - nsfs nsfs rw\n")
	fixture.host.facts.MountInfo = append(fixture.host.facts.MountInfo, line...)
	fixture.host.facts.MountInfoAfter = bytes.Clone(fixture.host.facts.MountInfo)
	if _, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host); err == nil {
		t.Fatal("namespace mount bypassed the nested-mount refusal")
	}
}

// Namespace spelling cannot excuse noncanonical ordinary roots or targets.
func TestRuntimeBoundaryMountsRejectMalformedNamespaceIdentity(t *testing.T) {
	for _, row := range []string{
		"3 1 0:42 net:[123456789] /run/synthetic-handle rw - ext4 synthetic rw\n",
		"3 1 0:42 net:[0] /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 net:[01] /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 net:[18446744073709551616] /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 net:[123 /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 unknown:[123] /run/synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 net:[123] /run/../synthetic-handle rw - nsfs nsfs rw\n",
		"3 1 0:42 relative /run/synthetic-handle rw - ext4 synthetic rw\n",
	} {
		if _, err := parseRuntimeBoundaryMounts([]byte(row), DefaultRuntimeResourceBoundarySettings()); err == nil {
			t.Fatalf("malformed mount identity accepted: %q", row)
		}
	}
}
