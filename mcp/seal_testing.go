package mcp

import (
	"crypto/cipher"
	"errors"
	"strings"
	"sync"
)

// Keys the process's seal cipher from secret in place of proxy.yml's first non-blank secret until the returned pop,
// for a test that runs mcp with keys of its own rather than the vault's. The seal key is the process's (seal.go), so
// a test sets it once, before mcp serves, for every tool call the process answers. A blank secret seals nothing, as
// a proxy.yml with none.
func Testing_SetSealSecret(secret string) func() {
	previous := sealAead
	sealAead = sync.OnceValues(func() (cipher.AEAD, error) {
		if strings.TrimSpace(secret) == "" {
			return nil, errors.New("no non-blank secret to derive the seal key from")
		}
		return sealAeadOfSecret(secret)
	})
	return func() {
		sealAead = previous
	}
}
