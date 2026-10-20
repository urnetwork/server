// Original history is an explicit bounded opt-in for existing proof consumers.
package handlers

import (
	"net/http/httptest"
	"testing"
)

// Legacy requests keep their original payload size; malformed options fail early.
func TestVerifyOriginalEvidenceRequiresExplicitOptIn(t *testing.T) {
	cases := []struct {
		query   string
		want    bool
		invalid bool
	}{
		{query: "", want: false},
		{query: "?originals=true", want: true},
		{query: "?originals=false", want: false},
		{query: "?originals=unexpected", invalid: true},
	}
	for index, c := range cases {
		args, err := verifyEvidenceArgs(httptest.NewRequest("GET", "https://operator.example/verify/proofs"+c.query, nil))
		if (err != nil) != c.invalid {
			t.Fatalf("case%d error=%v", index, err)
		}
		if err == nil && args.IncludeOriginals != c.want {
			t.Fatalf("case%d opt-in=%t", index, args.IncludeOriginals)
		}
	}
}
