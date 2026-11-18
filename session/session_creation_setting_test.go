package session

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// S1 W10: every mint read and parsed vault auth.yml for
// session_creation_enabled, and a file that failed to parse panicked the
// mint. The setting is now read at most once per interval, a delivery still
// takes effect without a restart, and a malformed file keeps the last value.
func TestSessionCreationSettingReadsOncePerIntervalAndSurvivesAMalformedFile(t *testing.T) {
	Testing_ResetSessionCreationSetting()
	defer Testing_ResetSessionCreationSetting()
	now := time.Date(2026, 11, 18, 19, 0, 0, 0, time.UTC)
	clock := sessionCreationSettingNow
	sessionCreationSettingNow = func() time.Time { return now }
	defer func() { sessionCreationSettingNow = clock }()

	pop := server.Vault.PushSimpleResource("auth.yml", []byte("session_creation_enabled: true\n"))
	if !SessionCreationEnabled() {
		t.Fatal("the delivered setting is not read")
	}
	pop()
	pop = server.Vault.PushSimpleResource("auth.yml", []byte("session_creation_enabled: false\n"))
	if !SessionCreationEnabled() {
		t.Fatal("the setting was read again within its interval")
	}
	now = now.Add(sessionCreationSettingInterval)
	if SessionCreationEnabled() {
		t.Fatal("a new delivery did not take effect after the interval")
	}
	pop()

	pop = server.Vault.PushSimpleResource("auth.yml", []byte("session_creation_enabled: true\n"))
	now = now.Add(sessionCreationSettingInterval)
	if !SessionCreationEnabled() {
		t.Fatal("the setting did not turn back on")
	}
	pop()
	pop = server.Vault.PushSimpleResource("auth.yml", []byte("session_creation_enabled: [true\n"))
	defer pop()
	now = now.Add(sessionCreationSettingInterval)
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("a malformed auth.yml panicked the mint: %v", recovered)
			}
		}()
		if !SessionCreationEnabled() {
			t.Fatal("a malformed auth.yml lost the last value")
		}
	}()
}

// The untagged mint count is labelled with the §15 session kind of the
// producer's registered kind.
func TestUntaggedMintKindIsTheSessionKind(t *testing.T) {
	network := &ByJwt{}
	client := (&ByJwt{}).Client(server.NewId(), server.NewId())
	for _, test := range []struct {
		kind       string
		credential *ByJwt
		want       string
	}{
		{"password", network, SessionKindSignin},
		{"signup", network, SessionKindSignin},
		{"auth_code", network, SessionKindCode},
		{"api_key_client", client, SessionKindClient},
		{"legacy_proxy", client, SessionKindClient},
		{"hosted_proxy", client, SessionKindClient},
		{"device_adopt", client, SessionKindClient},
		{"legacy", client, SessionKindClient},
		{"legacy", network, SessionKindSignin},
	} {
		if got := sessionKindClass(test.kind, test.credential); got != test.want {
			t.Errorf("kind %s of a %v credential counts as %s, want %s", test.kind, test.credential.ClientId != nil, got, test.want)
		}
	}
}
