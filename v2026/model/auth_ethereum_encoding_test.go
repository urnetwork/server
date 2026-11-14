package model

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestEthereumSignatureCompleteEncoding(t *testing.T) {
	data := bytes.Repeat([]byte{0x69}, 65)
	data[1] = 0xa0 // The base64 value starts with a valid but incomplete hex prefix.
	for _, encoded := range []string{hex.EncodeToString(data), "0x" + hex.EncodeToString(data), "0X" + hex.EncodeToString(data), base64.StdEncoding.EncodeToString(data), base64.RawURLEncoding.EncodeToString(data)} {
		if got := decodeSignatureBytes(encoded); !bytes.Equal(got, data) {
			t.Fatal("complete signature encoding did not round-trip")
		}
	}
	for _, bad := range []string{"", hex.EncodeToString(data) + "!", base64.StdEncoding.EncodeToString(data) + "!", hex.EncodeToString(data[:64]), "private-malformed-signature-canary"} {
		if decodeSignatureBytes(bad) != nil {
			t.Fatal("malformed signature encoding accepted")
		}
		if _, err := VerifyEthereumSignature("", "test", bad); err == nil || (bad != "" && strings.Contains(err.Error(), bad)) {
			t.Fatal("malformed signature must fail without reflecting its input")
		}
	}
}

func TestEthereumSignatureFormatsAndAddressCase(t *testing.T) {
	key, err := crypto.HexToECDSA(strings.Repeat("01", 32)) // Public synthetic fixture.
	if err != nil {
		t.Fatal(err)
	}
	message := "signature encoding integration control"
	hash := crypto.Keccak256Hash([]byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)))
	sig, err := crypto.Sign(hash.Bytes(), key)
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.PubkeyToAddress(key.PublicKey).Hex()
	for _, encoded := range []string{hex.EncodeToString(sig), base64.StdEncoding.EncodeToString(sig), base64.RawURLEncoding.EncodeToString(sig)} {
		for _, candidate := range []string{address, strings.ToLower(address)} {
			if ok, err := VerifyEthereumSignature(candidate, message, encoded); err != nil || !ok {
				t.Fatal("valid signature or lowercase address rejected")
			}
		}
		if ok, _ := VerifyEthereumSignature(address, message+" changed", encoded); ok {
			t.Fatal("signature accepted a changed message")
		}
	}
}
