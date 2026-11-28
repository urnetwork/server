// Exercises the same signed contract replies that the connect two-client TLS tests consume.
package controller

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// All material is synthetic. Fixed Ed25519 seeds let the source-side test use
// the corresponding identities without checking private key files into git.
func TestIssueContractsForRequiredSessionRekey(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := model.WithProviderWorkSessionSource(tb.Context(), nil)
		f := newContractEndpointReadFixture(ctx)
		ids := [2]server.Id{f.sourceId, f.destinationId}
		settings := connect.DefaultContractManagerSettings()
		settings.NetworkEventTimeChangeHmac = time.Time{}
		var identities [2]ed25519.PrivateKey
		for side, id := range ids {
			identities[side] = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(31 + side)}, ed25519.SeedSize))
			model.SetClientPublicKey(ctx, id, identities[side].Public().(ed25519.PublicKey))
			model.SetProvide(ctx, id, map[model.ProvideMode][]byte{model.ProvideModeNetwork: bytes.Repeat([]byte{byte(81 + side)}, 32)})
		}
		certificatePem := func(serial int64) []byte {
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(70 + serial)}, ed25519.SeedSize))
			cert := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "session.example"},
				NotBefore: time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
				KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			der, err := x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
			if err != nil {
				tb.Fatal(err)
			}
			return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		}
		issue := func(source int, leaf []byte) []byte {
			streamVersion := uint32(connect.DefaultStreamVersion)
			forceStream := false
			role := protocol.SequenceRole_SequenceRoleClient
			if source == 1 {
				role = protocol.SequenceRole_SequenceRoleServer
			}
			request := &protocol.CreateContract{
				DestinationId: ids[1-source].Bytes(), TransferByteCount: uint64(MinContractTransferByteCount),
				StreamVersion: &streamVersion, SenderRole: &role, ForceStream: &forceStream,
			}
			frames, err := CreateContract(ctx, ids[source], request, settings)
			defer returnConnectControlFrames(frames)
			if err != nil || len(frames) != 1 {
				tb.Fatalf("issue contract: frames=%d err=%v", len(frames), err)
			}
			message, err := connect.FromFrame(frames[0])
			if err != nil {
				tb.Fatal(err)
			}
			result, ok := message.(*protocol.CreateContractResult)
			if !ok || result.Error != nil || result.Contract == nil || !proto.Equal(result.CreateContract, request) {
				tb.Fatalf("expected a successful response echoing the request, got %v", message)
			}
			c := result.Contract
			stored := &protocol.StoredContract{}
			if err := proto.Unmarshal(c.StoredContractBytes, stored); err != nil {
				tb.Fatal(err)
			}
			key := identities[1-source].Public().(ed25519.PublicKey)
			if !bytes.Equal(stored.SourceId, ids[source].Bytes()) ||
				!bytes.Equal(stored.DestinationId, ids[1-source].Bytes()) ||
				stored.GetExpirationTimeUnixMilli() <= time.Now().UnixMilli() ||
				!bytes.Equal(stored.DestinationClientKeySignedTlsCertificate, c.DestinationClientKeySignedTlsCertificate) ||
				!bytes.Equal(bytes.Join(c.ProvideTlsCertificate, nil), leaf) ||
				!connect.VerifyStoredContract(settings, bytes.Repeat([]byte{byte(81 + 1 - source)}, 32), c.StoredContractBytes, c.StoredContractHmac) ||
				!bytes.Equal(stored.DestinationClientPublicKey, key) || c.ProvideMode != protocol.ProvideMode_Network ||
				!bytes.Equal(bytes.Join(stored.ProvideTlsCertificate, nil), leaf) ||
				(len(leaf) > 0 && !connect.VerifyCertChainSignature(key, stored.ProvideTlsCertificate, stored.DestinationClientKeySignedTlsCertificate)) {
				tb.Fatal("issued contract lost its destination, MAC or signed certificate")
			}
			wire, err := proto.Marshal(frames[0])
			if err != nil {
				tb.Fatal(err)
			}
			return wire
		}
		firstLeaf := certificatePem(1)
		model.SetClientTlsCertificateWithSignature(ctx, ids[1], firstLeaf, ed25519.Sign(identities[1], firstLeaf))
		initial := issue(0, firstLeaf)
		secondLeaf := certificatePem(2)
		// Both leaf certificates remain committed during the rollover.
		bundle := append(bytes.Clone(firstLeaf), secondLeaf...)
		model.SetClientTlsCertificateWithSignature(ctx, ids[1], bundle, ed25519.Sign(identities[1], bundle))
		rotation := issue(0, bundle)
		reply := issue(1, nil)
		path := os.Getenv("URNETWORK_TLS_CONTRACT_FIXTURE_OUT")
		if path == "" {
			return
		}
		out, err := json.MarshalIndent(struct{ Initial, Rotation, Reply []byte }{Initial: initial, Rotation: rotation, Reply: reply}, "", "  ")
		if err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(path, append(out, '\n'), 0600); err != nil {
			tb.Fatal(err)
		}
	})
}
