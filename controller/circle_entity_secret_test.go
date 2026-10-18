// Entity-secret configuration errors are ordinary refusals before any Circle
// mutation. Synthetic public-key reads exercise the real cipher path.
package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

func TestCircleEntitySecretMalformedHexReturnsError(t *testing.T) {
	defer func() {
		if value := recover(); value != nil {
			t.Errorf("malformed entity secret panicked instead of refusing: %v", value)
		}
	}()
	if encrypted, err := generateEntitySecretCipher(context.Background(), "synthetic-not-hex"); encrypted != nil || err == nil {
		t.Fatalf("malformed entity secret result = %x, %v", encrypted, err)
	}
}

// The existing per-client observation seam keeps keys and transport local.
func circleEntitySecretTestClient(t *testing.T, key *rsa.PublicKey, calls *int) *CoreCircleApiClient {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	return paymentReadTestCircle(paymentReadHooks{do: func(*http.Request) (*http.Response, error) {
		*calls++
		return paymentReadTestResponse(http.StatusOK, circleReadTestPage(t, "publicKey", encoded), nil), nil
	}})
}

func TestCircleEntitySecretRejectsInvalidSizeBeforeKeyRead(t *testing.T) {
	client := paymentReadTestCircle(paymentReadHooks{do: func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid secret reached the key endpoint")
		return nil, nil
	}})
	for _, size := range []int{0, 1, 31, 33, 512} {
		if encrypted, err := client.generateEntitySecretCipher(context.Background(), strings.Repeat("ab", size)); encrypted != nil || err == nil {
			t.Errorf("%d-byte secret result = %x, %v", size, encrypted, err)
		}
	}
}

func TestCircleEntitySecretCipherUsesConfiguredSecret(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := circleEntitySecretTestClient(t, &key.PublicKey, &calls)
	secret := strings.Repeat("ab", 32)
	encrypted, err := client.generateEntitySecretCipher(context.Background(), secret)
	if err != nil || len(encrypted) == 0 || calls != 1 {
		t.Fatalf("valid secret encryption = %x, %v; calls=%d", encrypted, err, calls)
	}
	plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, encrypted, nil)
	expected, decodeErr := hex.DecodeString(secret)
	if err != nil || decodeErr != nil || !bytes.Equal(plain, expected) {
		t.Fatal("ciphertext did not encrypt exactly the configured entity secret")
	}
}

// A syntactically valid public key can still be unusable by RSA-OAEP. That
// provider/configuration failure must be returned through the ordinary caller.
func TestCircleEntitySecretCipherFailureReturnsError(t *testing.T) {
	key := &rsa.PublicKey{N: new(big.Int).SetBit(new(big.Int), 511, 1), E: 65537}
	calls := 0
	client := circleEntitySecretTestClient(t, key, &calls)
	defer func() {
		if value := recover(); value != nil {
			t.Errorf("unusable RSA public key panicked instead of refusing: %v", value)
		}
	}()
	if encrypted, err := client.generateEntitySecretCipher(context.Background(), strings.Repeat("ab", 32)); encrypted != nil || err == nil || calls != 1 {
		t.Fatalf("unusable key result = %x, %v; calls=%d", encrypted, err, calls)
	}
}
