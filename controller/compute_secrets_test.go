package controller

// ComputeServerSecrets (BAL-M12-07): the ledger's two keys read from the process's vault file on each call, and every
// file that is not two distinct base64 keys of at least 32 bytes refused, naming no key's byte.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A compute-ledger.yml of the two keys.
func testComputeLedgerFile(payerKey []byte, promoKey []byte) []byte {
	return []byte("payer_hash_key: " + base64.StdEncoding.EncodeToString(payerKey) + "\n" +
		"promo_hash_key: " + base64.StdEncoding.EncodeToString(promoKey) + "\n")
}

func TestComputeServerSecretsReadTheVaultFile(t *testing.T) {
	secrets := NewComputeServerSecrets(nil)
	if _, err := secrets.PayerHashKey(); !errors.Is(err, ErrComputeLedgerSecrets) {
		t.Fatalf("no file answered %v", err)
	}
	payerKey := bytes.Repeat([]byte{0x5a}, 32)
	promoKey := bytes.Repeat([]byte{0xa5}, 40)
	pop := server.Vault.PushSimpleResource(ComputeLedgerSecretsResourceName, testComputeLedgerFile(payerKey, promoKey))
	readPayer, err := secrets.PayerHashKey()
	if err != nil || !bytes.Equal(readPayer, payerKey) {
		pop()
		t.Fatalf("the payer key: %v", err)
	}
	readPromo, err := secrets.PromoHashKey()
	pop()
	if err != nil || !bytes.Equal(readPromo, promoKey) {
		t.Fatalf("the promotion key: %v", err)
	}
	// a removed file is read again on the next call
	if _, err := secrets.PromoHashKey(); !errors.Is(err, ErrComputeLedgerSecrets) {
		t.Fatalf("a removed file answered %v", err)
	}
}

func TestComputeServerSecretsRefuseABadFile(t *testing.T) {
	good := bytes.Repeat([]byte{0x11}, 32)
	other := bytes.Repeat([]byte{0x22}, 32)
	for label, file := range map[string][]byte{
		"a short key":        testComputeLedgerFile(good, other[:31]),
		"one key for both":   testComputeLedgerFile(good, good),
		"no promotion key":   []byte("payer_hash_key: " + base64.StdEncoding.EncodeToString(good) + "\n"),
		"a blank key":        []byte("payer_hash_key: \"\"\npromo_hash_key: " + base64.StdEncoding.EncodeToString(other) + "\n"),
		"no base64":          []byte("payer_hash_key: not*base64*at*all\npromo_hash_key: " + base64.StdEncoding.EncodeToString(other) + "\n"),
		"a number for a key": []byte("payer_hash_key: 12345\npromo_hash_key: " + base64.StdEncoding.EncodeToString(other) + "\n"),
		"no mapping":         []byte("- payer_hash_key\n"),
	} {
		func() {
			pop := server.Vault.PushSimpleResource(ComputeLedgerSecretsResourceName, file)
			defer pop()
			secrets := NewComputeServerSecrets(nil)
			_, payerErr := secrets.PayerHashKey()
			_, promoErr := secrets.PromoHashKey()
			if !errors.Is(payerErr, ErrComputeLedgerSecrets) || !errors.Is(promoErr, ErrComputeLedgerSecrets) {
				t.Errorf("%s: %v, %v", label, payerErr, promoErr)
				return
			}
			// the error names the file, never a key's text
			for _, err := range []error{payerErr, promoErr} {
				if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(good)) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(other)) {
					t.Errorf("%s: the error carries a key: %v", label, err)
				}
			}
		}()
	}
}
