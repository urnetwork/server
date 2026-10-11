// The settlement v2 ledger endpoints are production routes beside /sn/artifact.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/router"
)

// SV-3: the production router serves GET /sn/ledger/object and GET
// /sn/ledger/account, classified public, to a caller without credentials,
// refusing a malformed reference or coldkey before any storage or chain read,
// and refuses other methods.
func TestSnLedgerRoutesArePublic(t *testing.T) {
	t.Parallel()
	routes := Routes()
	for _, route := range []string{"/sn/ledger/object", "/sn/ledger/account"} {
		count := 0
		for _, candidate := range routes {
			if candidate.String() == "GET ^"+route+"$" {
				count++
			}
		}
		if count != 1 || routeAccessByRoute["GET "+route] != routeAccessPublic {
			t.Fatalf("route GET %s census=%d access=%d, want 1 public route", route, count, routeAccessByRoute["GET "+route])
		}
	}
	handler := router.NewRouter(t.Context(), routes)
	for _, target := range []string{"/sn/ledger/object?hash=sha256:00", "/sn/ledger/account?coldkey=0x00"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(method, target, nil))
			want := http.StatusMethodNotAllowed
			if method == http.MethodGet {
				want = http.StatusBadRequest
			}
			if response.Code != want {
				t.Fatalf("%s %s: status %d, want %d", method, target, response.Code, want)
			}
		}
	}
}
