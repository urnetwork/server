// String resource identities retain their complete canonical representation.
// A separate typed namespace leaves every existing Id ownership key unchanged.
package server

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"
)

// Use the exact durable identity, such as a task's complete RunOnce.String(),
// without truncation or reconstruction from invocation arguments. Every writer
// must use the same domain and encoding. Hash collisions only over-serialize;
// the string namespace never intentionally aliases an Id's byte encoding.
func NewPgOwnershipKeyFromString(domain string, identity string) PgOwnershipKey {
	if domain == "" || strings.ContainsRune(domain, '\x00') || identity == "" {
		panic(errors.New("invalid database ownership string identity"))
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("urnetwork:business-owner-string:v1\x00"))
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(identity))
	sum := hash.Sum(nil)
	return PgOwnershipKey{first: int32(binary.BigEndian.Uint32(sum[:4])), second: int32(binary.BigEndian.Uint32(sum[4:8]))}
}
