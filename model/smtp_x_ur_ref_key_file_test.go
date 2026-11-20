package model

import (
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// smtpedge.yml's names for these tests.
func testSmtpXUrRefSettings() *smtpedgeservice.SmtpEdgeSettings {
	settings := smtpedgeservice.DefaultSmtpEdgeSettings()
	settings.Root = "root.example"
	settings.ReportHostName = "rpt.report.example"
	return settings
}

// The key file lays its keys and its issuing key over the settings, and a file the settings refuse, one that does
// not parse, a key id past one octet, a key that is not base64 or is short, or an issuing key it does not hold, is
// refused naming the part and never a key.
func TestSmtpXUrRefKeyFileLaysItsKeysOverTheSettings(t *testing.T) {
	settings := testSmtpXUrRefSettings()
	file := "issue_key_id: 2\nkeys:\n  - {id: 1, key: " + strings.Repeat("QUFB", 11) + "}\n  - {id: 2, key: " + strings.Repeat("QkJC", 11) + "}\n"
	if err := ApplySmtpXUrRefKeyFile(settings, []byte(file)); err != nil {
		t.Fatal(err)
	}
	if len(settings.XUrRefKeys) != 2 || settings.XUrRefIssueKeyId != 2 || settings.XUrRefKeys[1].KeyId != 2 || len(settings.XUrRefKeys[1].Key) != 33 {
		t.Fatalf("keys %d, issue %d", len(settings.XUrRefKeys), settings.XUrRefIssueKeyId)
	}
	secret := "c2VjcmV0LXZhbHVlLW5vdC1iYXNlNjQ!!"
	for _, bad := range []string{
		"issue_key_id: 3\nkeys:\n  - {id: 1, key: " + strings.Repeat("QUFB", 11) + "}\n",
		"issue_key_id: 1\nkeys:\n  - {id: 1, key: " + secret + "}\n",
		"issue_key_id: 1\nkeys:\n  - {id: 300, key: " + strings.Repeat("QUFB", 11) + "}\n",
		"issue_key_id: 1\nkeys:\n  - {id: 1, key: QUFB}\n",
		"issue_key_id: 1\nkeys: " + secret + "\n",
		"issue_key_id: 1\nkeys:\n  - {id: 1, key: " + strings.Repeat("QUFB", 11) + ", note: " + secret + "}\n",
	} {
		err := ApplySmtpXUrRefKeyFile(testSmtpXUrRefSettings(), []byte(bad))
		if !errors.Is(err, ErrSmtpXUrRefKeyFileRefused) {
			t.Fatalf("%q was taken: %v", bad, err)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "QUFB") || !strings.Contains(err.Error(), SmtpXUrRefKeyFileName) {
			t.Fatalf("a refusal named a key or no file: %v", err)
		}
	}
}

// The block's vault file: one the vault does not hold is refused as missing, and one it holds is laid over the
// settings.
func TestSmtpXUrRefKeyFileLoadsFromTheVault(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		err := LoadSmtpXUrRefKeyFile(testSmtpXUrRefSettings())
		if !errors.Is(err, ErrSmtpXUrRefKeyFileRefused) || !strings.Contains(err.Error(), "no "+SmtpXUrRefKeyFileName) {
			t.Fatalf("no file: %v", err)
		}
		pop := server.Vault.PushSimpleResource(SmtpXUrRefKeyFileName, []byte("issue_key_id: 1\nkeys:\n  - {id: 1, key: "+strings.Repeat("QUFB", 11)+"}\n"))
		defer pop()
		settings := testSmtpXUrRefSettings()
		if err := LoadSmtpXUrRefKeyFile(settings); err != nil {
			t.Fatal(err)
		}
		if len(settings.XUrRefKeys) != 1 || settings.XUrRefKeys[0].KeyId != 1 {
			t.Fatalf("keys %+v", settings.XUrRefKeys)
		}
	})
}
