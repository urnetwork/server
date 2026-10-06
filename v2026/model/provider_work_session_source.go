// The optional live source has its own explicit key. Payout signing authority
// never supplies this role, and absent configuration preserves ordinary traffic.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

const ProviderWorkSessionSourceSchema = "urnetwork-provider-work-session-source-v1"

// The authority is independently approved in the whole-work roster. The local
// source configuration alone cannot make any consumer trust this public key.
type ProviderWorkSessionSource struct {
	authority protocol.ProviderWorkSourceAuthority
	key       ed25519.PrivateKey
}

type providerWorkSessionSourceContextKey struct{}

// Own the key and public policy before entering any transaction callback.
func NewProviderWorkSessionSource(authority protocol.ProviderWorkSourceAuthority, key ed25519.PrivateKey) (*ProviderWorkSessionSource, error) {
	if err := authority.Validate(); err != nil {
		return nil, err
	}
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) || authority.PublicKey != [32]byte(key.Public().(ed25519.PublicKey)) || authority.DomainHash == ([32]byte{}) || authority.SourceId == "" || authority.Generation == "" || authority.FromUnixMicro <= 0 || authority.ThroughUnixMicro <= authority.FromUnixMicro || authority.MaxEndpointEvents == 0 {
		return nil, errors.New("provider work session source authority or key is invalid")
	}
	authority.DirectoryPublicKeys = append([][32]byte(nil), authority.DirectoryPublicKeys...)
	return &ProviderWorkSessionSource{authority: authority, key: bytes.Clone(key)}, nil
}

// An instance-scoped owner also supports deterministic source fixtures without
// replacing process globals or teaching the payout signer another authority.
func WithProviderWorkSessionSource(ctx context.Context, source *ProviderWorkSessionSource) context.Context {
	return context.WithValue(ctx, providerWorkSessionSourceContextKey{}, source)
}

// Pin one read for this live operation. An unavailable or invalid optional role
// leaves an unsigned journal gap; it never guesses a fallback private key.
func providerWorkSessionContext(ctx context.Context) context.Context {
	if ctx.Value(providerWorkSessionSourceContextKey{}) != nil {
		return ctx
	}
	source, _ := loadProviderWorkSessionSource()
	return WithProviderWorkSessionSource(ctx, source)
}

func providerWorkSessionSourceFromContext(ctx context.Context) *ProviderWorkSessionSource {
	source, _ := ctx.Value(providerWorkSessionSourceContextKey{}).(*ProviderWorkSessionSource)
	return source
}

// This dedicated vault resource is intentionally independent of st.yml and the
// ordinary artifact key. Strict finite decoding refuses accidental key reuse.
func loadProviderWorkSessionSource() (*ProviderWorkSessionSource, error) {
	resource, err := server.Vault.SimpleResource("provider_work_session.json")
	if err != nil {
		return nil, err
	}
	raw, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	return providerWorkSessionSourceFromBytes(raw)
}

// Return only public source identity after the same finite parsing and signer
// checks as the live loader. No environment, filesystem, cache, or signing
// operation is used. The caller still owns independent roster approval and
// resource custody; missing bytes are an error, never complete source coverage.
func InspectProviderWorkSessionSourceBytes(raw []byte) (protocol.ProviderWorkSourceAuthority, error) {
	source, err := providerWorkSessionSourceFromBytes(raw)
	if err != nil {
		return protocol.ProviderWorkSourceAuthority{}, err
	}
	clear(source.key)
	return source.authority, nil
}

// Share the exact live decoder and constructor. Static failures cannot echo
// secret JSON values or unknown field names; decoded temporary key bytes are
// cleared after the constructor takes its independent copy for a live owner.
func providerWorkSessionSourceFromBytes(raw []byte) (*ProviderWorkSessionSource, error) {
	if len(raw) == 0 || len(raw) > 64*1024 {
		return nil, errors.New("provider work source configuration exceeds capacity")
	}
	var policy struct {
		Schema     string                               `json:"schema"`
		Authority  protocol.ProviderWorkSourceAuthority `json:"authority"`
		PrivateKey []byte                               `json:"private_key"`
	}
	defer func() { clear(policy.PrivateKey) }()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return nil, errors.New("provider work source configuration cannot be decoded")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || policy.Schema != ProviderWorkSessionSourceSchema {
		return nil, errors.New("provider work source configuration is not canonical")
	}
	source, err := NewProviderWorkSessionSource(policy.Authority, ed25519.PrivateKey(policy.PrivateKey))
	if err != nil {
		return nil, errors.New("provider work source configuration authority or key is invalid")
	}
	return source, nil
}

// Check the local event time as well as the independently verified consumer
// window. Expired producer configuration can never mint a fresh original.
func (self *ProviderWorkSessionSource) sign(ctx context.Context, value protocol.ProviderWorkReceipt, at int64) (protocol.ProviderWorkReceipt, []byte, [32]byte, error) {
	if self == nil || at < self.authority.FromUnixMicro || at >= self.authority.ThroughUnixMicro {
		return value, nil, [32]byte{}, errors.New("provider work source is outside its original authority window")
	}
	value.DomainHash = self.authority.DomainHash
	value.SourceId = self.authority.SourceId
	value.Generation = self.authority.Generation
	value.PublicKey = self.authority.PublicKey
	signed, err := protocol.SignProviderWorkReceipt(ctx, value, self.key)
	if err != nil {
		return value, nil, [32]byte{}, err
	}
	raw, err := signed.Bytes(ctx)
	if err != nil {
		return value, nil, [32]byte{}, err
	}
	hash, err := signed.ContentHash(ctx)
	return signed, raw, hash, err
}
