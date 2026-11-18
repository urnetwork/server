package session

import (
	"testing"

	"github.com/urnetwork/server"
)

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
