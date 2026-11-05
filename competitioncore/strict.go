// Control documents reject duplicate/case-folded authority and unknown fields
// before any process classification or durable registry update.
package competitioncore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Nested token work is bounded independently of decoded array lengths.
func strictDocument(content []byte, value any, settings Settings) error {
	if settings.DocumentBytes <= 0 || int64(len(content)) > settings.DocumentBytes || settings.DocumentDepth <= 0 || settings.DocumentTokens <= 0 {
		return errors.New("unbounded core control document")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	count := 0
	var read func(int) error
	read = func(depth int) error {
		count++
		if count > settings.DocumentTokens || depth > settings.DocumentDepth {
			return errors.New("core control document exceeds its finite grammar")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		seen := map[string]bool{}
		for decoder.More() {
			if delimiter == '{' {
				token, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || key != strings.ToLower(key) || seen[key] {
					return errors.New("duplicate or case-folded core authority")
				}
				seen[key] = true
			}
			if err := read(depth + 1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || delimiter != '{' && delimiter != '[' || delimiter == '{' && end != json.Delim('}') || delimiter == '[' && end != json.Delim(']') {
			return errors.New("malformed core control document")
		}
		return nil
	}
	if err := read(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing core control document")
	}
	decoder = json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

// Digests bind exact public control bytes, never core contents.
func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// All identities are lower-case canonical hexadecimal rather than prefixes.
func validHex(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
