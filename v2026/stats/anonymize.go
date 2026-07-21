package stats

// id anonymization for exported stats.
//
// Stats streams are published (goal 10), so ids in them must not be matchable
// to production ids. But the same real id must map to the same pseudonym
// across every sample so a client/provider can be traced through the stats.
// The pseudonym is HMAC-SHA256(salt, id)[:16] with a fixed salt shared by
// every instance (never rotated automatically — rotation would sever traces
// across the rotation boundary; it is a deliberate manual op if the salt ever
// leaks).
//
// In WARP_ENV=local the raw id bytes are used verbatim, so a simulation can
// join exported samples against its own ground truth (providers.yml).

import (
	"crypto/hmac"
	"crypto/sha256"

	"github.com/urnetwork/server/v2026"
)

// anonymizer maps a real id to a stable pseudonym.
type anonymizer struct {
	// nil in raw (local) mode
	salt []byte
}

// newAnonymizer resolves the anonymization mode. In local env it returns a
// raw-mode anonymizer. Otherwise it requires a non-empty salt; ok is false
// when the salt is missing so the caller can fail safe (disable stats rather
// than risk leaking raw production ids).
func newAnonymizer(local bool, salt []byte) (a *anonymizer, ok bool) {
	if local {
		return &anonymizer{salt: nil}, true
	}
	if len(salt) == 0 {
		return nil, false
	}
	return &anonymizer{salt: salt}, true
}

// anonymize returns the 16-byte pseudonym for id.
func (self *anonymizer) anonymize(id server.Id) []byte {
	idBytes := id.Bytes()
	if self.salt == nil {
		// raw mode: the pseudonym is the id itself
		out := make([]byte, len(idBytes))
		copy(out, idBytes)
		return out
	}
	mac := hmac.New(sha256.New, self.salt)
	mac.Write(idBytes)
	sum := mac.Sum(nil)
	return sum[:16]
}
