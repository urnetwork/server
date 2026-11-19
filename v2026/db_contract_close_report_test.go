package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestCloseReportMigrationKeepsDeployed763Prefix(t *testing.T) {
	if MigrationCount() < 764 || migrations[763].(*SqlMigration).sql != contractCloseReportSchemaSql {
		t.Fatal("close-report schema did not append at reserved migration764")
	}
	ids := make([]string, 763)
	for i := range ids {
		var err error
		ids[i], err = MigrationIdentity(i)
		if err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(encoded)
	if hex.EncodeToString(hash[:]) != "4c3f6c68627ace67ef9e2e73ac0a5e9e5ce16e7ad5969e72003956475c3406ac" {
		t.Fatal("deployed canonical migrations1–763 changed")
	}
}
