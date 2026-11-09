package session

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The sidecar's DPoP signer against this verifier (§7.4, CORE-M1-04): the
// proofs sidecar/testdata/dpop/vectors.json pins byte for byte, made on every
// sidecar leg by the ClientKey signer that SidecarApi installs, verify here
// with their own key, method, uri and token at their iat, and with any one of
// those changed they do not. Pure.

// sidecar's vectors, beside this checkout in the monoroot.
const dpopVectorsPath = "../../sidecar/testdata/dpop/vectors.json"

type dpopVector struct {
	Name      string `json:"name"`
	Seed      string `json:"seed"`
	Method    string `json:"method"`
	Url       string `json:"url"`
	Token     string `json:"token"`
	Iat       int64  `json:"iat"`
	PublicKey string `json:"public_key"`
	Ath       string `json:"ath"`
	Htu       string `json:"htu"`
	Jti       string `json:"jti"`
	Proof     string `json:"proof"`
}

func readDpopVectors(t testing.TB) []*dpopVector {
	t.Helper()
	data, err := os.ReadFile(dpopVectorsPath)
	if err != nil {
		t.Fatalf("sidecar's dpop vectors: %v", err)
	}
	vectors := struct {
		Proofs []*dpopVector `json:"proofs"`
	}{}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Proofs) == 0 {
		t.Fatal("no dpop vectors")
	}
	return vectors.Proofs
}

// The check a server makes of the vector: the uri it is configured with is
// where the vector sent its request.
func (self *dpopVector) check(t testing.TB) *DpopRequest {
	t.Helper()
	seed, err := hex.DecodeString(self.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("%s: seed", self.Name)
	}
	publicKey := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	connect.AssertEqual(t, hex.EncodeToString(publicKey), self.PublicKey)
	return &DpopRequest{
		Proof:    self.Proof,
		Method:   self.Method,
		Uri:      self.Url,
		Token:    self.Token,
		Key:      publicKey,
		ReplayId: server.NewId(),
	}
}

func (self *dpopVector) iat() time.Time {
	return time.Unix(self.Iat, 0).UTC()
}

// Each pinned proof verifies with its key at its iat and returns its jti, and its
// ath is SHA-256 of its token as this verifier computes it.
func TestDpopProofVectorsVerify(t *testing.T) {
	for _, vector := range readDpopVectors(t) {
		hash := sha256.Sum256([]byte(vector.Token))
		connect.AssertEqual(t, base64.RawURLEncoding.EncodeToString(hash[:]), vector.Ath)

		jti, err := verifyDpopProof(vector.check(t), vector.iat(), DefaultDpopProofSettings())
		if err != nil {
			t.Fatalf("%s: %v", vector.Name, err)
		}
		connect.AssertEqual(t, jti, vector.Jti)

		// the thumbprint form names the same key
		check := vector.check(t)
		check.KeyThumbprint = DpopKeyThumbprint(check.Key)
		check.Key = nil
		if _, err := verifyDpopProof(check, vector.iat(), DefaultDpopProofSettings()); err != nil {
			t.Fatalf("%s by thumbprint: %v", vector.Name, err)
		}
	}
}

// A pinned proof proves its own request only: another method, uri, token, key or
// time refuses it, and so does a flipped byte anywhere in it.
func TestDpopProofVectorsBindTheirRequest(t *testing.T) {
	otherKey, _ := newDpopTestKey(t)
	for _, vector := range readDpopVectors(t) {
		for name, edit := range map[string]func(check *DpopRequest){
			"another method": func(check *DpopRequest) { check.Method = "PUT" },
			"another path":   func(check *DpopRequest) { check.Uri = strings.TrimSuffix(vector.Htu, "/") + "x" },
			"another host": func(check *DpopRequest) {
				parsed, err := url.Parse(vector.Htu)
				connect.AssertEqual(t, err, nil)
				parsed.Host = "other.example"
				check.Uri = parsed.String()
			},
			"another token": func(check *DpopRequest) { check.Token += "x" },
			"another key":   func(check *DpopRequest) { check.Key = otherKey },
		} {
			check := vector.check(t)
			edit(check)
			_, err := verifyDpopProof(check, vector.iat(), DefaultDpopProofSettings())
			requireDpopRefused(t, vector.Name+": "+name, err)
		}
		for _, now := range []time.Time{vector.iat().Add(-61 * time.Second), vector.iat().Add(61 * time.Second)} {
			_, err := verifyDpopProof(vector.check(t), now, DefaultDpopProofSettings())
			requireDpopRefused(t, vector.Name+": out of its window", err)
		}
		for i := 0; i < len(vector.Proof); i += 1 {
			if vector.Proof[i] == '.' {
				continue
			}
			check := vector.check(t)
			flipped := []byte(check.Proof)
			if flipped[i] == 'A' {
				flipped[i] = 'B'
			} else {
				flipped[i] = 'A'
			}
			check.Proof = string(flipped)
			if _, err := verifyDpopProof(check, vector.iat(), DefaultDpopProofSettings()); err == nil {
				// a flip in the last character of a base64url part may land on bits
				// the encoding drops, leaving the same bytes
				if decodedEqual(vector.Proof, check.Proof) {
					continue
				}
				t.Fatalf("%s: a flip at %d verified", vector.Name, i)
			}
		}
	}
}

// Whether two compact JWS decode to the same bytes part by part.
func decodedEqual(a string, b string) bool {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	if len(aParts) != len(bParts) {
		return false
	}
	for i := range aParts {
		aBytes, aErr := base64.RawURLEncoding.DecodeString(aParts[i])
		bBytes, bErr := base64.RawURLEncoding.DecodeString(bParts[i])
		if aErr != nil || bErr != nil || string(aBytes) != string(bBytes) {
			return false
		}
	}
	return true
}

// Any input either refuses or verifies as the request it names: the verifier
// never panics, and nothing but the vector's key, method, uri, token and window
// gets through. The seeds, the vectors and their mutations, run as ordinary
// tests.
func FuzzDpopProof(f *testing.F) {
	vectors := readDpopVectors(f)
	for _, vector := range vectors {
		f.Add(vector.Proof)
		parts := strings.Split(vector.Proof, ".")
		f.Add(parts[0] + "." + parts[1] + ".")
		f.Add(parts[0] + "." + parts[1])
		f.Add(strings.Repeat(parts[0]+".", 3))
		f.Add("eyJhbGciOiJub25lIn0." + parts[1] + ".")
	}
	f.Add("")
	f.Add("..")
	f.Add("not a proof")
	vector := vectors[0]
	f.Fuzz(func(t *testing.T, proof string) {
		check := vector.check(t)
		check.Proof = proof
		jti, err := verifyDpopProof(check, vector.iat(), DefaultDpopProofSettings())
		if err != nil {
			requireDpopRefused(t, "fuzzed", err)
			return
		}
		// it verified, so its claims name the vector's request and token
		claims := &dpopProofClaims{}
		if _, _, err := gojwt.NewParser().ParseUnverified(proof, claims); err != nil {
			t.Fatalf("verified but does not parse: %v", err)
		}
		if claims.ID != jti || claims.Htm != vector.Method || !dpopHtuMatches(claims.Htu, vector.Url) || claims.Ath != vector.Ath {
			t.Fatalf("verified claims %+v for another request", claims)
		}
	})
}
