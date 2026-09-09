// Independent legacy Json/signature oracles and allocation-volume regressions
// exercise real server entrypoints, not a replaceable observer-only fast path.
package startifact

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urnetwork/server"
)

// Own generated synthetic identity and sign with the original full encoder.
func evidenceEncodingEnvelopeTest(t *testing.T, payload json.RawMessage) (*EvidenceEnvelope, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	envelope := &EvidenceEnvelope{
		Schema: EvidenceSchema, DeploymentID: "synthetic-stream-deployment",
		ChainID: 1337, GenesisHash: "0x" + strings.Repeat("1", 64), Netuid: 7,
		Kind: "synthetic-stream", RunID: "synthetic-run",
		CreatedAt: "synthetic-<>&-\"payload\":null",
		Payload:   bytes.Clone(payload), Signer: crypto.PubkeyToAddress(key.PublicKey),
	}
	unsigned, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(unsigned)
	signature, err := crypto.Sign(digest[:], key)
	if err != nil {
		t.Fatal(err)
	}
	envelope.ContentHash = "sha256:" + hex.EncodeToString(digest[:])
	envelope.Signature = "0x" + hex.EncodeToString(signature)
	return envelope, key
}

func TestEvidenceEncodingPreservesLegacyCanonicalBytesAndSignatures(t *testing.T) {
	t.Parallel()
	for _, payload := range []json.RawMessage{
		[]byte("null"),
		[]byte(" \t [true,false,null,-0,1e+02,1E-0002,0.1000] \r\n"),
		[]byte(" { \"same\":1,\"same\":2,\"\\u0061\":3,\"a\":4 } "),
		[]byte("{\"s\":\"<>&\u2028\u2029\\u003C\\uD800\\uDFFF\\/\\\\\\\"\"}"),
		{'"', 0xff, 0xc0, 0xaf, '"'},
	} {
		envelope, key := evidenceEncodingEnvelopeTest(t, payload)
		before := *envelope
		before.Payload = bytes.Clone(envelope.Payload)
		want, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := VerifyEvidence(envelope); err != nil {
				t.Fatal("fresh verifier rejected original full-envelope signature", err)
			}
			got, err := EvidenceBytes(envelope)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("publication changed original canonical wire", err)
			}
		}
		if !reflect.DeepEqual(envelope, &before) {
			t.Fatal("verification or wire encoding mutated borrowed identity or payload")
		}
		resigned := *envelope
		if err := SignEvidence(&resigned, key); err != nil || resigned.ContentHash != envelope.ContentHash || resigned.Signature != envelope.Signature {
			t.Fatal("stream signer changed deterministic legacy authority", err)
		}
	}
}

func TestEvidenceEncodingRejectsMutableOrUnauthenticatedInputs(t *testing.T) {
	t.Parallel()
	envelope, key := evidenceEncodingEnvelopeTest(t, []byte("{\"value\":1}"))
	for _, mutate := range []func(*EvidenceEnvelope){
		func(value *EvidenceEnvelope) { value.Payload[len(value.Payload)-2] = '2' },
		func(value *EvidenceEnvelope) { value.Payload = []byte("{") },
		func(value *EvidenceEnvelope) { value.Payload = []byte("null null") },
		func(value *EvidenceEnvelope) { value.Signature = "0x" + strings.Repeat("00", 65) },
		func(value *EvidenceEnvelope) { value.ContentHash = "sha256:" + strings.Repeat("0", 64) },
		func(value *EvidenceEnvelope) { value.Schema += "-other" },
		func(value *EvidenceEnvelope) { value.DeploymentID = "../escape" },
		func(value *EvidenceEnvelope) { value.RunID = "_deployment" },
		func(value *EvidenceEnvelope) { value.GenesisHash = "0x0" },
		func(value *EvidenceEnvelope) { value.CreatedAt = "" },
		func(value *EvidenceEnvelope) { value.ChainID++ },
		func(value *EvidenceEnvelope) { value.Netuid++ },
		func(value *EvidenceEnvelope) { value.Kind += "-changed" },
		func(value *EvidenceEnvelope) { value.Signer[0] ^= 1 },
	} {
		changed := *envelope
		changed.Payload = bytes.Clone(envelope.Payload)
		mutate(&changed)
		if wire, err := EvidenceBytes(&changed); err == nil || wire != nil {
			t.Fatal("unauthenticated input escaped the real wire publication boundary")
		}
		if err := VerifyEvidence(&changed); err == nil {
			t.Fatal("fresh verifier accepted mutated identity or bytes")
		}
	}
	envelope.Payload[len(envelope.Payload)-2] = '2'
	if wire, err := EvidenceBytes(envelope); err == nil || wire != nil {
		t.Fatal("same backing owner retained authority after mutation")
	}
	envelope.Payload[len(envelope.Payload)-2] = '1'
	if err := VerifyEvidence(envelope); err != nil {
		t.Fatal("restored original did not authenticate afresh", err)
	}
	if err := SignEvidence(nil, key); err == nil {
		t.Fatal("nil envelope was signed")
	}
	if err := VerifyEvidence(nil); err == nil {
		t.Fatal("nil envelope verified")
	}
	if wire, err := EvidenceBytes(nil); err == nil || wire != nil {
		t.Fatal("nil envelope acquired a wire owner")
	}
}

// Sparse canonical payloads mirror binary carrier data; dense escaping probes
// the same bounded verifier without making elapsed time an assertion.
func TestEvidenceEncodingSignAndVerifyKeepAllocationBelowPayload(t *testing.T) {
	for _, value := range []string{
		strings.Repeat("x", 1024*1024),
		strings.Repeat("<>&\u2028\u2029", 128*1024),
	} {
		envelope, key := evidenceEncodingEnvelopeTest(t, []byte("\""+value+"\""))
		for _, operation := range []func() error{
			func() error { return VerifyEvidence(envelope) },
			func() error { return SignEvidence(envelope, key) },
		} {
			var operationErr error
			result := testing.Benchmark(func(b *testing.B) {
				for range b.N {
					operationErr = operation()
					if operationErr != nil {
						b.Fatal(operationErr)
					}
				}
			})
			if operationErr != nil || result.N == 0 {
				t.Fatal("actual sign/verify allocation measurement failed", operationErr)
			}
			if allocated := result.AllocedBytesPerOp(); allocated <= 0 || allocated >= int64(len(envelope.Payload))/4 {
				t.Fatalf("real sign/verify allocated a payload-sized copy: allocated=%d payload=%d", allocated, len(envelope.Payload))
			}
		}
	}
}

// The required returned wire is allowed; an additional unsigned envelope or
// canonical payload owner is not. This fails the original two-Marshal path.
func TestEvidenceEncodingPublicationKeepsOnlyReturnedWire(t *testing.T) {
	envelope, _ := evidenceEncodingEnvelopeTest(t, []byte("\""+strings.Repeat("x", 1024*1024)+"\""))
	want, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var wire []byte
	var encodeErr error
	result := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			wire, encodeErr = EvidenceBytes(envelope)
			if encodeErr != nil {
				b.Fatal(encodeErr)
			}
		}
	})
	if encodeErr != nil || !bytes.Equal(wire, want) || result.N == 0 {
		t.Fatal("real publication did not retain its exact original wire", encodeErr)
	}
	maximum := int64(len(want) + len(envelope.Payload)/4)
	if allocated := result.AllocedBytesPerOp(); allocated <= 0 || allocated >= maximum {
		t.Fatalf("real publication retained another envelope-sized encoding: allocated=%d maximum=%d", allocated, maximum)
	}
}

// Both immutable-create and later replica verification preserve simultaneous
// reader and Close failures while discharging the real owned descriptor.
func TestPreparedEvidenceStreamingPreservesJoinedReadAndCloseFailures(t *testing.T) {
	t.Parallel()
	envelope, _ := evidenceEncodingEnvelopeTest(t, []byte("{\"value\":1}"))
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	readFailure := errors.New("synthetic read refusal")
	closeFailure := errors.New("synthetic close refusal")
	store := &preparedEvidenceStoreTest{
		BlobStore: server.NewLocalBlobStore(t.TempDir(), "synthetic-operator"),
		readErr:   readFailure, closeErr: closeFailure,
	}
	if _, err := prepared.Publish(t.Context(), store); !errors.Is(err, readFailure) || !errors.Is(err, closeFailure) || store.closes != 1 {
		t.Fatal("immutable-create readback lost a reader or Close failure", err)
	}
	store.readErr, store.closeErr = nil, nil
	published, err := prepared.Publish(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	before := store.closes
	store.readErr, store.closeErr = readFailure, closeFailure
	if err := prepared.VerifyPublished(t.Context(), store, published); !errors.Is(err, readFailure) || !errors.Is(err, closeFailure) || store.closes != before+1 {
		t.Fatal("replica readback lost a reader or Close failure", err)
	}
}

// Real disk replicas still execute both complete content/history reads.
// Allocation is fixed-buffer volume, independent of carrier length.
func TestPreparedEvidenceStreamingReadbackKeepsAllocationBelowPayload(t *testing.T) {
	envelope, _ := evidenceEncodingEnvelopeTest(t, []byte("\""+strings.Repeat("x", 1024*1024)+"\""))
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "synthetic-operator")}
	published, err := prepared.Publish(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	var verifyErr error
	result := testing.Benchmark(func(b *testing.B) {
		reads, closes := store.gets, store.closes
		for range b.N {
			verifyErr = prepared.VerifyPublished(t.Context(), store, published)
			if verifyErr != nil {
				b.Fatal(verifyErr)
			}
		}
		if store.gets-reads != 2*b.N || store.closes-closes != 2*b.N {
			b.Fatal("replica verification omitted a route read or Close")
		}
	})
	if verifyErr != nil || result.N == 0 {
		t.Fatal("real replica allocation measurement failed", verifyErr)
	}
	if allocated := result.AllocedBytesPerOp(); allocated <= 0 || allocated >= int64(len(envelope.Payload))/4 {
		t.Fatalf("replica readback allocated carrier-sized copies: allocated=%d payload=%d", allocated, len(envelope.Payload))
	}
}
