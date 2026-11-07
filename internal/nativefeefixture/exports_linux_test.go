//go:build linux

package nativefeefixture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The real build: sn's mainnet.test from the sibling checkout that this server
// module builds against, run as the gate runs it, writes the three manifests
// that the public tests settle, each pinning that image by its path and
// SHA-256. It builds whatever the gate's variables say, as Require does when
// none is set.
func TestProvisionBuildsTheGateExportsFromTheSiblingSn(t *testing.T) {
	exports, err := provision(t, runtime.GOOS, defaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyExports(exports); err != nil {
		t.Fatal(err)
	}
	var transactions [][]byte
	for _, name := range exportManifests {
		raw, err := os.ReadFile(filepath.Join(exports.FixtureDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			Schema         string `json:"schema"`
			RawTransaction []byte `json:"raw_transaction"`
			Request        struct {
				Path string `json:"path"`
			} `json:"request"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.Schema != "urnetwork-native-fee-settlement-test-fixture-v1" || len(manifest.RawTransaction) == 0 || filepath.Dir(filepath.Dir(manifest.Request.Path)) != exports.FixtureDirectory {
			t.Fatalf("%s is not an original export in %s: %s", name, exports.FixtureDirectory, raw)
		}
		transactions = append(transactions, manifest.RawTransaction)
	}
	for _, transaction := range transactions[1:] {
		if !bytes.Equal(transaction, transactions[0]) {
			t.Fatal("the exports lost their one original signed transaction")
		}
	}
}
