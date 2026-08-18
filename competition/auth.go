package competition

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

type Principal struct {
	Id   string
	Role string
}

func Authenticate(request *http.Request, settings *Settings) (*Principal, bool) {
	const prefix = "Bearer "
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) || strings.Contains(header[len(prefix):], " ") {
		return nil, false
	}
	raw := header[len(prefix):]
	if raw == "" || len(raw) > 1024 {
		return nil, false
	}
	digest := sha256.Sum256([]byte(raw))
	var matched *Token
	for i := range settings.Tokens {
		expected, err := hex.DecodeString(settings.Tokens[i].Sha256)
		if err != nil || len(expected) != sha256.Size {
			continue
		}
		if subtle.ConstantTimeCompare(digest[:], expected) == 1 {
			matched = &settings.Tokens[i]
		}
	}
	if matched == nil {
		return nil, false
	}
	return &Principal{Id: matched.Name, Role: matched.Role}, true
}
