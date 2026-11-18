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
		{"password", network, sessionKindSignin},
		{"signup", network, sessionKindSignin},
		{"auth_code", network, sessionKindCode},
		{"api_key_client", client, sessionKindClient},
		{"legacy_proxy", client, sessionKindClient},
		{"hosted_proxy", client, sessionKindClient},
		{"device_adopt", client, sessionKindClient},
		{"legacy", client, sessionKindClient},
		{"legacy", network, sessionKindSignin},
		{sessionKindCode, network, sessionKindCode},
	} {
		if got := sessionKindClass(test.kind, test.credential); got != test.want {
			t.Errorf("kind %s of a %v credential counts as %s, want %s", test.kind, test.credential.ClientId != nil, got, test.want)
		}
	}
}
