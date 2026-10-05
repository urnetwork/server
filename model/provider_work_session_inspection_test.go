// Explicit source inspection shares the live finite parser while returning
// public authority only. All identities and keys here are synthetic and offline.
package model

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urfoundation/sn/protocol"
)

// The explicit direct-only authority exercises the constructor's valid zero
// cohort bound without borrowing any payout or traffic-signing role.
type providerWorkInspectionConfig struct {
	Schema     string                               `json:"schema"`
	Authority  protocol.ProviderWorkSourceAuthority `json:"authority"`
	PrivateKey []byte                               `json:"private_key"`
}

// Fixed test seeds and UUIDs make every decoded public field reproducible.
func newProviderWorkInspectionConfig() providerWorkInspectionConfig {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x41}, ed25519.SeedSize))
	return providerWorkInspectionConfig{
		Schema: ProviderWorkSessionSourceSchema,
		Authority: protocol.ProviderWorkSourceAuthority{
			DomainHash: [32]byte{0x42}, SourceId: "00000000-0000-0000-0000-000000000101",
			Generation: "00000000-0000-0000-0000-000000000102", PublicKey: [32]byte(key.Public().(ed25519.PublicKey)),
			FromUnixMicro: 1, ThroughUnixMicro: 100, MaxEndpointEvents: 4, MaxCohortMembers: 0,
			DirectoryPublicKeys: [][32]byte{{0x43}, {0x44}},
		},
		PrivateKey: key,
	}
}

// Only finite original bytes are given to either parser entry point.
func providerWorkInspectionBytes(t testing.TB, value providerWorkInspectionConfig) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Live resource loading and explicit inspection select identical authority,
// and inspection must not clear the live owner's independently copied key.
func TestInspectProviderWorkSessionSourceBytesMatchesLiveLoader(t *testing.T) {
	config := newProviderWorkInspectionConfig()
	raw := providerWorkInspectionBytes(t, config)
	root := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-selected")
	if err := os.WriteFile(filepath.Join(root, "provider_work_session.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := loadProviderWorkSessionSource()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(source.key)
	authority, err := InspectProviderWorkSessionSourceBytes(raw)
	if err != nil || !reflect.DeepEqual(authority, source.authority) || !reflect.DeepEqual(authority, config.Authority) {
		t.Fatalf("inspection authority differs from live loader: %v", err)
	}
	if !bytes.Equal(source.key, config.PrivateKey) {
		t.Fatal("inspection or temporary cleanup changed the live owner's key")
	}
	encoded, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private_key", base64.StdEncoding.EncodeToString(config.PrivateKey), hex.EncodeToString(config.PrivateKey)} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("public authority serialization contains private key material")
		}
	}
}

// Inspecting bytes cannot read the process-selected vault, mutate the borrowed
// input, or share a mutable public-key slice across independent inspections.
func TestInspectProviderWorkSessionSourceBytesOwnsExplicitInput(t *testing.T) {
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_ENV", "synthetic-absent")
	config := newProviderWorkInspectionConfig()
	raw := providerWorkInspectionBytes(t, config)
	retained := bytes.Clone(raw)
	authority, err := InspectProviderWorkSessionSourceBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	authority.DirectoryPublicKeys[0][0] = 0x55
	second, err := InspectProviderWorkSessionSourceBytes(raw)
	if err != nil || !reflect.DeepEqual(second, config.Authority) || !bytes.Equal(raw, retained) {
		t.Fatalf("explicit inspection borrowed mutable state or changed input: %v", err)
	}
	clear(raw)
	if !reflect.DeepEqual(second, config.Authority) {
		t.Fatal("public authority retained borrowed raw source bytes")
	}
}

// Both entry points must reject authorities or private keys refused by the
// actual constructor, including a self-consistent key for a different signer.
func TestInspectProviderWorkSessionSourceBytesUsesActualKeyValidation(t *testing.T) {
	for _, mutate := range []func(*providerWorkInspectionConfig){
		func(config *providerWorkInspectionConfig) { config.PrivateKey = nil },
		func(config *providerWorkInspectionConfig) { config.PrivateKey = config.PrivateKey[:ed25519.SeedSize] },
		func(config *providerWorkInspectionConfig) { config.PrivateKey[len(config.PrivateKey)-1] ^= 1 },
		func(config *providerWorkInspectionConfig) {
			config.PrivateKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x56}, ed25519.SeedSize))
		},
		func(config *providerWorkInspectionConfig) { config.Authority.DomainHash = [32]byte{} },
		func(config *providerWorkInspectionConfig) { config.Authority.SourceId = "synthetic-invalid-identity" },
		func(config *providerWorkInspectionConfig) { config.Authority.Generation = "" },
		func(config *providerWorkInspectionConfig) {
			config.Authority.ThroughUnixMicro = config.Authority.FromUnixMicro
		},
		func(config *providerWorkInspectionConfig) { config.Authority.MaxEndpointEvents = 0 },
		func(config *providerWorkInspectionConfig) {
			config.Authority.DirectoryPublicKeys[1] = config.Authority.DirectoryPublicKeys[0]
		},
	} {
		config := newProviderWorkInspectionConfig()
		mutate(&config)
		if _, err := NewProviderWorkSessionSource(config.Authority, ed25519.PrivateKey(config.PrivateKey)); err == nil {
			t.Fatal("invalid fixture is accepted by the actual constructor")
		}
		raw := providerWorkInspectionBytes(t, config)
		authority, err := InspectProviderWorkSessionSourceBytes(raw)
		if err == nil || !reflect.DeepEqual(authority, protocol.ProviderWorkSourceAuthority{}) {
			t.Fatalf("invalid source returned a public declaration: %v", err)
		}
		if source, err := providerWorkSessionSourceFromBytes(raw); err == nil || source != nil {
			t.Fatalf("shared live parser accepted invalid source: %v", err)
		}
	}
}

// Unknown field names can themselves carry private bytes. Strict parsing must
// refuse them and return only static messages on both live and public paths.
func TestInspectProviderWorkSessionSourceBytesRedactsStrictParseErrors(t *testing.T) {
	config := newProviderWorkInspectionConfig()
	raw := providerWorkInspectionBytes(t, config)
	secret := base64.StdEncoding.EncodeToString(config.PrivateKey)
	unknownField := []byte(strings.TrimSuffix(string(raw), "}") + ",\"" + secret + "\":true}")
	unknownAuthorityField := bytes.Replace(raw, []byte(`"authority":{`), []byte(`"authority":{"`+secret+`":true,`), 1)
	invalidKey := bytes.Replace(raw, []byte(secret), []byte("synthetic-private-input-is-not-base64"), 1)
	wrongSchema := bytes.Replace(raw, []byte(ProviderWorkSessionSourceSchema), []byte("synthetic-unknown-schema"), 1)
	for _, invalid := range [][]byte{unknownField, unknownAuthorityField, invalidKey, wrongSchema, append(bytes.Clone(raw), []byte("\n{}")...), append(bytes.Clone(raw), '!')} {
		authority, err := InspectProviderWorkSessionSourceBytes(invalid)
		if err == nil || !reflect.DeepEqual(authority, protocol.ProviderWorkSourceAuthority{}) {
			t.Fatalf("invalid JSON returned a public declaration: %v", err)
		}
		source, liveErr := providerWorkSessionSourceFromBytes(invalid)
		if source != nil || liveErr == nil || liveErr.Error() != err.Error() {
			t.Fatalf("live and public parser rejection differ: %v / %v", liveErr, err)
		}
		for _, privateValue := range []string{secret, "synthetic-private-input-is-not-base64"} {
			if strings.Contains(err.Error(), privateValue) {
				t.Fatal("source parsing error echoed private input")
			}
		}
	}
}

// Optional absence is handled by the resolver, never by treating missing or
// excessive supplied bytes as a valid empty source authority.
func TestInspectProviderWorkSessionSourceBytesPreservesFiniteMissingBoundary(t *testing.T) {
	config := newProviderWorkInspectionConfig()
	raw := providerWorkInspectionBytes(t, config)
	atCapacity := append(bytes.Clone(raw), bytes.Repeat([]byte{' '}, 64*1024-len(raw))...)
	if authority, err := InspectProviderWorkSessionSourceBytes(atCapacity); err != nil || !reflect.DeepEqual(authority, config.Authority) {
		t.Fatalf("valid exact-capacity source was rejected: %v", err)
	}
	for _, invalid := range [][]byte{nil, {}, []byte(" \n"), []byte("{}"), []byte("null"), append(atCapacity, ' ')} {
		authority, err := InspectProviderWorkSessionSourceBytes(invalid)
		if err == nil || !reflect.DeepEqual(authority, protocol.ProviderWorkSourceAuthority{}) {
			t.Fatalf("absent or excessive input became public coverage: %v", err)
		}
	}
}
