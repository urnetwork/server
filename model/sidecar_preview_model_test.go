package model

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/server"
)

// The preview gate (sidecar_preview_model.go). Pure: sidecar.yml comes from the
// test's own override.

// An absent file holds no preview network, so it refuses; a listed network is
// allowed and an unlisted one refused; a list that names the network among bad
// entries still names it.
func TestSidecarPreviewAllowedOnlyForAListedNetwork(t *testing.T) {
	networkId := server.NewId()

	func() {
		defer server.Testing_PushSidecarConfig("")()
		if SidecarPreviewAllowed(networkId) {
			t.Fatal("an absent sidecar.yml previews a network")
		}
	}()
	func() {
		defer server.Testing_PushSidecarConfig("sidecar:\n  preview_networks: []\n")()
		if SidecarPreviewAllowed(networkId) {
			t.Fatal("an empty list previews a network")
		}
	}()
	func() {
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", server.NewId()))()
		if SidecarPreviewAllowed(networkId) {
			t.Fatal("another network's listing previews this one")
		}
	}()
	func() {
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [\"not-an-id\", %q]\n", networkId))()
		if !SidecarPreviewAllowed(networkId) {
			t.Fatal("a listed network is refused")
		}
	}()
}

// A sidecar.yml that is there but cannot be read previews nothing on a process's
// first read, so the gate fails closed on a bad push.
func TestSidecarPreviewRefusesWhenTheFileCannotBeRead(t *testing.T) {
	// forget this test's read once the home is the process's again, so no other
	// test answers from it (cleanups run last first)
	forgetRead := func() {
		server.Testing_PushSidecarConfig("")()
	}
	t.Cleanup(forgetRead)

	networkId := server.NewId()
	home := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// a directory where the file should be: there, but not readable as a file
	if err := os.Mkdir(filepath.Join(home, "sidecar.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_CONFIG_HOME", home)
	forgetRead()
	if SidecarPreviewAllowed(networkId) {
		t.Fatal("an unreadable sidecar.yml previews a network")
	}
}
