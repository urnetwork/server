package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/gossip"

	"github.com/urnetwork/server/v2026/session"
)

// The extender vault resource and what hello serves from it
// (connect/EXTENDER.md B1, B4, C7, A5).

func TestExtenderConfigAllowedHosts(t *testing.T) {
	cases := []struct {
		name         string
		config       *ExtenderConfig
		allowedHosts []string
	}{
		{
			name:         "the primary host alone",
			config:       &ExtenderConfig{NetworkHost: "ur.example"},
			allowedHosts: []string{"ur.example", "*.ur.example"},
		},
		{
			name: "a migration host as well",
			config: &ExtenderConfig{
				NetworkHost:  "ur.example",
				NetworkHosts: []string{"ur.example", "old.example"},
			},
			allowedHosts: []string{
				"ur.example",
				"*.ur.example",
				"old.example",
				"*.old.example",
			},
		},
		{
			name: "normalized and deduplicated",
			config: &ExtenderConfig{
				NetworkHost:  " UR.example. ",
				NetworkHosts: []string{"ur.example", "", "UR.EXAMPLE"},
			},
			allowedHosts: []string{"ur.example", "*.ur.example"},
		},
		{
			name:         "nothing configured",
			config:       &ExtenderConfig{},
			allowedHosts: []string{},
		},
	}
	for _, c := range cases {
		allowedHosts := c.config.AllowedHosts()
		if !slices.Equal(allowedHosts, c.allowedHosts) {
			t.Errorf("%s: allowed hosts = %v, want %v", c.name, allowedHosts, c.allowedHosts)
		}
	}
}

// A key that is not an ed25519 key is dropped rather than served: a client
// cannot tell a typo from a key it is supposed to trust, so it must never see
// one.
func TestExtenderConfigRootPublicKeysDropsWhatIsNotAKey(t *testing.T) {
	goodKeyHex := hex.EncodeToString(make([]byte, ed25519.PublicKeySize))
	config := &ExtenderConfig{
		RootPublicKeysHex: []string{
			goodKeyHex,
			"",
			"   ",
			"not-hex",
			hex.EncodeToString(make([]byte, 16)),
		},
	}
	rootPublicKeys := config.RootPublicKeys()
	if !slices.Equal(rootPublicKeys, []string{goodKeyHex}) {
		t.Fatalf("root public keys = %v, want only the ed25519 key", rootPublicKeys)
	}
}

func TestExtenderConfigRootPrivateKey(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := (&ExtenderConfig{}).RootPrivateKey(); err == nil {
		t.Fatal("an empty root key must be an error, not a zero key")
	}
	if _, err := (&ExtenderConfig{RootPrivateKeyHex: "zz"}).RootPrivateKey(); err == nil {
		t.Fatal("an unreadable root key must be an error")
	}

	config := &ExtenderConfig{RootPrivateKeyHex: connect.ExtenderKeySeedHex(seed)}
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	expectPublicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if !rootPrivateKey.Public().(ed25519.PublicKey).Equal(expectPublicKey) {
		t.Fatal("the derived key does not match the seed")
	}
}

func TestExtenderConfigApiHost(t *testing.T) {
	cases := []struct {
		apiUrl  string
		apiHost string
		wantErr bool
	}{
		{apiUrl: "https://api.ur.example", apiHost: "api.ur.example"},
		{apiUrl: "https://api.ur.example:8443", apiHost: "api.ur.example"},
		{apiUrl: "", wantErr: true},
		{apiUrl: "https://", wantErr: true},
		{apiUrl: "://", wantErr: true},
	}
	for _, c := range cases {
		apiHost, err := (&ExtenderConfig{ApiUrl: c.apiUrl}).ApiHost()
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error, got %q", c.apiUrl, apiHost)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.apiUrl, err)
			continue
		}
		if apiHost != c.apiHost {
			t.Errorf("%q: api host = %q, want %q", c.apiUrl, apiHost, c.apiHost)
		}
	}
}

// The gossip identity is a seed like every other ed25519 identity, and
// anything that is not one is an error rather than a key the service would
// silently run under (C6).
func TestExtenderConfigGossipIdentityKeySeed(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}

	malformed := []string{
		"",
		"   ",
		"zz",
		// a full seed's worth of hex characters, one nibble short
		strings.Repeat("a", 2*ed25519.SeedSize-1),
		// a key rather than a seed is still the wrong length here
		hex.EncodeToString(make([]byte, 16)),
	}
	for _, gossipIdentityKeyHex := range malformed {
		config := &ExtenderConfig{GossipIdentityKeyHex: gossipIdentityKeyHex}
		if _, err := config.GossipIdentityKeySeed(); err == nil {
			t.Errorf("gossip identity %q must be an error", gossipIdentityKeyHex)
		}
		if _, err := config.GossipPeerId(); err == nil {
			t.Errorf("gossip peer id of %q must be an error", gossipIdentityKeyHex)
		}
	}

	// whitespace is trimmed, which is what a yaml value with a stray space is
	config := &ExtenderConfig{GossipIdentityKeyHex: " " + connect.ExtenderKeySeedHex(seed) + " "}
	gossipIdentityKeySeed, err := config.GossipIdentityKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gossipIdentityKeySeed, seed) {
		t.Fatal("the parsed seed does not match the configured one")
	}
}

// The peer id hello serves must be exactly the one the mesh derives from the
// same key, since a member demands that id at the other end of its operator
// dial (C7, D3).
func TestExtenderConfigGossipPeerIdMatchesTheMeshDerivation(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	expectPeerId, err := gossip.PeerIdForExtenderPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}

	config := &ExtenderConfig{GossipIdentityKeyHex: connect.ExtenderKeySeedHex(seed)}
	gossipPeerId, err := config.GossipPeerId()
	if err != nil {
		t.Fatal(err)
	}
	if gossipPeerId != expectPeerId.String() {
		t.Fatalf("gossip peer id = %q, want %q", gossipPeerId, expectPeerId)
	}
}

// A probe fronts the extender with a name it cannot recognize as the
// operator's, so the extender cannot behave differently for a probe. The
// bundled spoof list decides the shape: with a list, the name is one of its
// domains and needs no network host; with an empty list, it is a random label
// under the operator's own host and every probe gets a different one, since a
// constant name would be just as recognizable. Neither shape is ever the
// operator's own host.
func TestExtenderConfigProbeServerNameIsRandomUnderTheNetworkHost(t *testing.T) {
	config := &ExtenderConfig{NetworkHost: "ur.example"}
	spoofDomains := connect.SpoofDomains()

	if 0 < len(spoofDomains) {
		for range 8 {
			serverName, err := config.ProbeServerName()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(spoofDomains, serverName) {
				t.Fatalf("probe name %q is not a bundled spoof domain", serverName)
			}
			if serverName == config.NetworkHost || strings.HasSuffix(serverName, "."+config.NetworkHost) {
				t.Fatalf("probe name %q is the operator's own host", serverName)
			}
		}
		// a spoof name is independent of the network, so it needs no host
		if _, err := (&ExtenderConfig{}).ProbeServerName(); err != nil {
			t.Fatal(err)
		}
		return
	}

	seen := map[string]bool{}
	for range 8 {
		serverName, err := config.ProbeServerName()
		if err != nil {
			t.Fatal(err)
		}
		if serverName == config.NetworkHost {
			t.Fatalf("probe name %q is the operator's own host", serverName)
		}
		if !strings.HasSuffix(serverName, ".ur.example") {
			t.Fatalf("probe name %q is not under the network host", serverName)
		}
		if label := strings.TrimSuffix(serverName, ".ur.example"); label == "" {
			t.Fatalf("probe name %q has no label", serverName)
		}
		seen[serverName] = true
	}
	if len(seen) != 8 {
		t.Fatalf("probe names repeat: %d distinct of 8", len(seen))
	}

	if _, err := (&ExtenderConfig{}).ProbeServerName(); err == nil {
		t.Fatal("a probe name with no host must be an error")
	}
}

// Hello serves the configured accepted keys. This is the only way a client
// learns what to trust, and it travels on the platform's pinned tls, so it is
// safe even when the request itself went through an untrusted extender (B4).
func TestHelloServesTheConfiguredExtenderRootKeys(t *testing.T) {
	firstKeyHex := hex.EncodeToString(make([]byte, ed25519.PublicKeySize))
	secondKey := make([]byte, ed25519.PublicKeySize)
	secondKey[0] = 1
	secondKeyHex := hex.EncodeToString(secondKey)

	installTestExtenderConfigYaml(t, strings.Join([]string{
		"root_public_keys_hex:",
		"  - " + firstKeyHex,
		"  - " + secondKeyHex,
		"network_host: ur.example",
	}, "\n"))

	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	result, err := Hello(clientSession)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.ExtenderRootPublicKeys, []string{firstKeyHex, secondKeyHex}) {
		t.Fatalf("hello keys = %v", result.ExtenderRootPublicKeys)
	}
	// this operator runs no gossip service, so there is no peer id to dial
	if result.GossipPeerId != "" {
		t.Fatalf("hello gossip peer id = %q, want none", result.GossipPeerId)
	}
}

// Hello serves the operator's mesh identity, which is what a member needs
// before it can make the one dial that gets it into the mesh (C7, D3).
func TestHelloServesTheConfiguredGossipPeerId(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	expectPeerId, err := gossip.PeerIdForExtenderPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}

	installTestExtenderConfigYaml(t, strings.Join([]string{
		"root_public_keys_hex:",
		"  - " + hex.EncodeToString(publicKey),
		"network_host: ur.example",
		"gossip_identity_key_hex: " + connect.ExtenderKeySeedHex(seed),
	}, "\n"))

	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	result, err := Hello(clientSession)
	if err != nil {
		t.Fatal(err)
	}
	if result.GossipPeerId != expectPeerId.String() {
		t.Fatalf("hello gossip peer id = %q, want %q", result.GossipPeerId, expectPeerId)
	}
}

// An operator with no extender network answers hello without the field at all,
// rather than failing hello, which every client calls before anything else.
func TestHelloServesNoExtenderKeysWhenUnconfigured(t *testing.T) {
	Testing_ResetExtenderConfig()
	t.Cleanup(Testing_ResetExtenderConfig)

	if _, err := EnvExtenderConfig(); err == nil {
		t.Fatal("this test requires an environment with no extender.yml")
	}

	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	result, err := Hello(clientSession)
	if err != nil {
		t.Fatalf("hello must not fail without an extender network: %v", err)
	}
	if len(result.ExtenderRootPublicKeys) != 0 {
		t.Fatalf("hello keys = %v, want none", result.ExtenderRootPublicKeys)
	}
	if result.GossipPeerId != "" {
		t.Fatalf("hello gossip peer id = %q, want none", result.GossipPeerId)
	}
	if result.ClientAddress == "" {
		t.Fatal("hello must still answer with the client address")
	}
}
