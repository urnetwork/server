package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/urnetwork/server/v2026"
)

func TestArinConnectLazyReadersReleaseAndReopenWithinRuntime(t *testing.T) {
	w, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "urnetwork arindb", IncludeReservedNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	_, prefix, _ := net.ParseCIDR("192.0.2.0/24")
	if err = w.Insert(prefix, mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String("subscriber"), "non_quality": mmdbtype.Bool(false), "risk": mmdbtype.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if _, err = w.WriteTo(&data); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.mmdb")
	hash := sha256.Sum256(data.Bytes())
	pin := hex.EncodeToString(hash[:])
	config := &server.ArinShadowRuntimeConfig{ActivePath: path, ActiveSHA256: pin, CandidatePath: path, CandidateSHA256: pin, Capacity: 8}
	handler, closeOwner, err := newArinShadowConnectRPC(context.Background(), config)
	if err != nil {
		t.Fatal("disabled-until-capture factory touched missing candidate", err)
	}
	defer closeOwner()
	if _, err = handler(context.Background(), "inventory", json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing resource invented")
	}
	if os.WriteFile(path, data.Bytes(), 0600) != nil {
		t.Fatal("fixture")
	}
	if _, err = handler(context.Background(), "inventory", json.RawMessage(`{}`)); err != nil {
		t.Fatal("real mapping failed", err)
	}
	if _, err = handler(context.Background(), "release", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// The already-mapped inode stays valid if a path disappears. Therefore a
	// missing-path error here proves release discarded its reader handles.
	if os.Rename(path, path+".saved") != nil {
		t.Fatal("rename")
	}
	if _, err = handler(context.Background(), "inventory", json.RawMessage(`{}`)); err == nil {
		t.Fatal("release retained old mapping")
	}
	if os.Rename(path+".saved", path) != nil {
		t.Fatal("restore")
	}
	if _, err = handler(context.Background(), "inventory", json.RawMessage(`{}`)); err != nil {
		t.Fatal("next capture needed reconnect", err)
	}
	closeOwner()
	if _, err = handler(context.Background(), "inventory", json.RawMessage(`{}`)); err == nil {
		t.Fatal("runtime close reopened readers")
	}
}
