package session

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
)

// The intended network check (intended_network.go). Pure: tokens are signed
// with the fixture vault's key and the API key lookup is replaced, so nothing
// reads the database or redis.

type intendedNetworkFixture struct {
	networkId    server.Id
	networkToken string
	clientToken  string
	// another network: the one a stale screen still shows
	otherNetworkId server.Id
}

func newIntendedNetworkFixture() *intendedNetworkFixture {
	networkId := server.NewId()
	network := NewByJwt(networkId, server.NewId(), "intended-network", false, false)
	return &intendedNetworkFixture{
		networkId:      networkId,
		networkToken:   network.Testing_Sign(),
		clientToken:    network.Client(server.NewId(), server.NewId()).Testing_Sign(),
		otherNetworkId: server.NewId(),
	}
}

func intendedNetworkRequest(authorization string, intended ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/auth/network-delete", nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	for _, value := range intended {
		r.Header.Add(IntendedNetworkIdHeader, value)
	}
	return r
}

// setIntendedNetworkApiKeyLookup replaces the API key lookup for one test.
func setIntendedNetworkApiKeyLookup(t *testing.T, lookup func(ctx context.Context, apiKey string) *apikey.NetworkByApiKey) {
	t.Helper()
	previous := intendedNetworkApiKeyLookup
	intendedNetworkApiKeyLookup = lookup
	t.Cleanup(func() {
		intendedNetworkApiKeyLookup = previous
	})
}

func refuseApiKeyLookup(t *testing.T) {
	setIntendedNetworkApiKeyLookup(t, func(ctx context.Context, apiKey string) *apikey.NetworkByApiKey {
		t.Fatal("the check looked an API key up")
		return nil
	})
}

func requireIntendedNetworkRefusal(t *testing.T, name string, err error, status int, code string) {
	t.Helper()
	var refusal *IntendedNetworkError
	if !errors.As(err, &refusal) {
		t.Fatalf("%s: err = %v, want the %s refusal", name, err, code)
	}
	if refusal.Status != status || refusal.Code != code {
		t.Fatalf("%s: refusal = %d %s, want %d %s", name, refusal.Status, refusal.Code, status, code)
	}
	// the router reads the status off the message (RaiseHttpError)
	if !strings.HasPrefix(err.Error(), strconv.Itoa(status)+" ") {
		t.Fatalf("%s: message %q does not lead with the status %d", name, err.Error(), status)
	}
	body, _ := refusal.HttpErrorResultBody().(map[string]any)
	if body["code"] != code || body["error"] != refusal.Message || refusal.Message == "" {
		t.Fatalf("%s: body = %v", name, body)
	}
}

// The header names exactly one network id, in the form the jwt's network_id
// claim has, or in the other form server.ParseId reads. Anything else asks for
// a check that cannot be made, and is refused rather than ignored.
func TestRequestIntendedNetworkId(t *testing.T) {
	networkId := server.NewId()
	canonical := networkId.String()
	header := func(values ...string) http.Header {
		h := http.Header{}
		for _, value := range values {
			h.Add(IntendedNetworkIdHeader, value)
		}
		return h
	}

	if _, ok, err := RequestIntendedNetworkId(header()); ok || err != nil {
		t.Fatalf("no header: ok = %t, err = %v; want no intended network", ok, err)
	}
	for name, h := range map[string]http.Header{
		"canonical":   header(canonical),
		"upper case":  header(strings.ToUpper(canonical)),
		"no dashes":   header(strings.ReplaceAll(canonical, "-", "")),
		"white space": header(" " + canonical + " "),
		// a browser sends header names in lower case over HTTP/2; the server
		// reads them into canonical names
		"lower case header name": func() http.Header {
			raw := "POST /auth/network-delete HTTP/1.1\r\nHost: api\r\n" + strings.ToLower(IntendedNetworkIdHeader) + ": " + canonical + "\r\n\r\n"
			r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
			if err != nil {
				t.Fatal(err)
			}
			return r.Header
		}(),
	} {
		got, ok, err := RequestIntendedNetworkId(h)
		if !ok || err != nil || got != networkId {
			t.Fatalf("%s: got %s ok = %t err = %v, want %s", name, got, ok, err, networkId)
		}
	}
	for name, h := range map[string]http.Header{
		"empty":       header(""),
		"not an id":   header("network-a"),
		"zero id":     header(server.Id{}.String()),
		"two headers": header(canonical, server.NewId().String()),
		"same twice":  header(canonical, canonical),
		"a list":      header(canonical + ", " + server.NewId().String()),
		"a name":      header("my-network.ur.network"),
	} {
		_, ok, err := RequestIntendedNetworkId(h)
		if ok {
			t.Fatalf("%s: accepted", name)
		}
		requireIntendedNetworkRefusal(t, name, err, http.StatusBadRequest, InvalidIntendedNetworkIdCode)
	}
}

// The wire contract the clients send: the header name and the refusal codes.
func TestIntendedNetworkWireContract(t *testing.T) {
	if IntendedNetworkIdHeader != "X-UR-Intended-Network-Id" {
		t.Fatalf("header = %q", IntendedNetworkIdHeader)
	}
	if IntendedNetworkMismatchCode != "intended_network_mismatch" || InvalidIntendedNetworkIdCode != "invalid_intended_network_id" {
		t.Fatalf("codes = %q %q", IntendedNetworkMismatchCode, InvalidIntendedNetworkIdCode)
	}
}

// The defect: a stale screen still showing network A sends network B's
// credential. With the header naming A, the check refuses B's network token
// and B's client token with 412. With the header naming B, both pass.
func TestCheckIntendedNetworkRefusesAnotherNetworksToken(t *testing.T) {
	fixture := newIntendedNetworkFixture()
	refuseApiKeyLookup(t)
	ctx := context.Background()

	for name, token := range map[string]string{
		"network token": fixture.networkToken,
		"client token":  fixture.clientToken,
	} {
		err := CheckIntendedNetwork(ctx, intendedNetworkRequest("Bearer "+token, fixture.otherNetworkId.String()))
		requireIntendedNetworkRefusal(t, name+" of another network", err, http.StatusPreconditionFailed, IntendedNetworkMismatchCode)

		if err := CheckIntendedNetwork(ctx, intendedNetworkRequest("Bearer "+token, fixture.networkId.String())); err != nil {
			t.Fatalf("%s of the intended network: err = %v, want none", name, err)
		}
	}
}

// Without the header nothing is checked and nothing is looked up, so a client
// that does not send it is served exactly as before.
func TestCheckIntendedNetworkWithoutTheHeaderChecksNothing(t *testing.T) {
	fixture := newIntendedNetworkFixture()
	refuseApiKeyLookup(t)
	for name, authorization := range map[string]string{
		"network token": "Bearer " + fixture.networkToken,
		"client token":  "Bearer " + fixture.clientToken,
		"api key":       "Bearer urn_" + strings.Repeat("k", 52),
		"no credential": "",
		"not a token":   "Bearer not.a.jwt",
	} {
		if err := CheckIntendedNetwork(context.Background(), intendedNetworkRequest(authorization)); err != nil {
			t.Fatalf("%s: err = %v, want none", name, err)
		}
	}
}

// A credential that does not verify, or no credential, is never refused by the
// check: the handler's authentication refuses it as it would without the
// header, so a bad credential stays its 401.
func TestCheckIntendedNetworkLeavesAnUnverifiedCredentialToTheHandler(t *testing.T) {
	fixture := newIntendedNetworkFixture()
	refuseApiKeyLookup(t)
	parts := strings.Split(fixture.networkToken, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	// the same claims under a different signature
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 64)))
	if claimed, ok := byJwtNetworkIdUnverified(forged); !ok || claimed != fixture.networkId {
		t.Fatalf("the forged token's claims name %s (%t), want %s", claimed, ok, fixture.networkId)
	}

	for name, request := range map[string]*http.Request{
		"forged token, another network": intendedNetworkRequest("Bearer "+forged, fixture.otherNetworkId.String()),
		"forged token, its own network": intendedNetworkRequest("Bearer "+forged, fixture.networkId.String()),
		"not a token":                   intendedNetworkRequest("Bearer not.a.jwt", fixture.otherNetworkId.String()),
		"no credential":                 intendedNetworkRequest("", fixture.otherNetworkId.String()),
		"other scheme":                  intendedNetworkRequest("Basic "+base64.StdEncoding.EncodeToString([]byte("a:b")), fixture.otherNetworkId.String()),
		"api key of the wrong length":   intendedNetworkRequest("Bearer urn_short", fixture.otherNetworkId.String()),
	} {
		if err := CheckIntendedNetwork(context.Background(), request); err != nil {
			t.Fatalf("%s: err = %v, want the handler to decide", name, err)
		}
	}
}

// An API key signs in to the network it was created for. The check looks it up
// as authentication does, refuses a key of another network, and leaves a key
// that names no network to the handler.
func TestCheckIntendedNetworkResolvesAnApiKey(t *testing.T) {
	keyNetworkId := server.NewId()
	apiKey := "urn_" + strings.Repeat("k", 52)
	lookups := 0
	known := true
	setIntendedNetworkApiKeyLookup(t, func(ctx context.Context, key string) *apikey.NetworkByApiKey {
		lookups += 1
		if key != apiKey {
			t.Fatalf("looked up %q, want the request's key", key)
		}
		if !known {
			return nil
		}
		return &apikey.NetworkByApiKey{NetworkId: keyNetworkId, UserId: server.NewId(), NetworkName: "keyed"}
	})
	ctx := context.Background()

	err := CheckIntendedNetwork(ctx, intendedNetworkRequest("Bearer "+apiKey, server.NewId().String()))
	requireIntendedNetworkRefusal(t, "api key of another network", err, http.StatusPreconditionFailed, IntendedNetworkMismatchCode)
	if err := CheckIntendedNetwork(ctx, intendedNetworkRequest("Bearer "+apiKey, keyNetworkId.String())); err != nil {
		t.Fatalf("api key of the intended network: err = %v", err)
	}
	known = false
	if err := CheckIntendedNetwork(ctx, intendedNetworkRequest("Bearer "+apiKey, server.NewId().String())); err != nil {
		t.Fatalf("unknown api key: err = %v, want the handler to refuse it", err)
	}
	if lookups != 3 {
		t.Fatalf("lookups = %d, want 3", lookups)
	}
}

// When the credential's network cannot be resolved because a dependency
// failed, the request is refused as authentication would refuse it (503), not
// served unchecked. Any other panic is not the check's to hide.
func TestCheckIntendedNetworkRefusesWhenTheNetworkCannotBeResolved(t *testing.T) {
	apiKey := "urn_" + strings.Repeat("k", 52)
	setIntendedNetworkApiKeyLookup(t, func(ctx context.Context, key string) *apikey.NetworkByApiKey {
		panic(context.DeadlineExceeded)
	})
	err := CheckIntendedNetwork(context.Background(), intendedNetworkRequest("Bearer "+apiKey, server.NewId().String()))
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("err = %v, want ErrAuthUnavailable", err)
	}

	setIntendedNetworkApiKeyLookup(t, func(ctx context.Context, key string) *apikey.NetworkByApiKey {
		panic(errors.New("not a dependency failure"))
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the check swallowed a panic that is not a dependency failure")
			}
		}()
		CheckIntendedNetwork(context.Background(), intendedNetworkRequest("Bearer "+apiKey, server.NewId().String()))
	}()
}
