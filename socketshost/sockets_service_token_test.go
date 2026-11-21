// T3 for the block's own Authenticator (QD-1029) on the private test Redis that test-env.sh attests: real service
// tokens of audience sockets, minted under a test svc- key as the exchange mints them, verified by the block against
// that key's public jwks as oauth-jwks-svc.json gives it, and §7.8's rev/ objects, signed as api signs them, on the
// block's first metadata cluster (here sidecartest's fake blobs). The block answers GET /socket/list, which reads only
// the store once the caller is authenticated, so its answer is the authentication's: a listing, 401 or unavailable.

package socketshost

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
)

// The test's issuer and svc- key in place of auth.yml's, and the process's verifier of that key, as a block's
// mounted oauth-jwks-svc.json gives it. The verifier's clock is returned for the expiry case.
func installServiceTokenTestKeys(t testing.TB) *oauth.ServiceTokenVerifier {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oauth.Testing_SetIssuer("https://auth.example"))
	t.Cleanup(oauth.Testing_SetSignerKeys(&oauth.SignerKey{
		Kid:        oauth.SignerKeySetService.KidPrefix() + thumbprint,
		Alg:        "ES256",
		Set:        oauth.SignerKeySetService,
		PrivateKey: privateKey,
	}))
	verifier, err := oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
		Issuer: "https://auth.example",
		Keys:   oauth.PublicJwks(oauth.SignerKeySetService).Keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oauth.Testing_SetServiceTokenVerifier(verifier))
	return verifier
}

// A token of audience for a fresh network's client, bound to a sidecar of a member, and its ids.
type serviceTokenTestCaller struct {
	token     string
	expiry    time.Time
	networkId server.Id
	clientId  server.Id
	sidecarId server.Id
	memberId  server.Id
}

func mintServiceTokenTestCaller(t testing.TB, audience string) *serviceTokenTestCaller {
	t.Helper()
	self := &serviceTokenTestCaller{networkId: server.NewId(), clientId: server.NewId(), sidecarId: server.NewId(), memberId: server.NewId()}
	token, expiry, err := oauth.MintServiceToken(&oauth.MintServiceTokenArgs{
		NetworkId: self.networkId,
		ClientId:  self.clientId,
		SidecarId: &self.sidecarId,
		MemberId:  &self.memberId,
		Audience:  audience,
	})
	if err != nil {
		t.Fatal(err)
	}
	self.token, self.expiry = token, expiry
	return self
}

// Writes the rev/ object that voids kind's subject in networkId for every token issued before notBefore, signed
// under the test's svc- key as api's rev/ writer signs it.
func revokeServiceTokenTestSubject(t testing.TB, blobs *sidecartest.FakeBlobs, networkId server.Id, kind sidecar.RevocationKind, subjectId server.Id, notBefore time.Time) {
	t.Helper()
	record := &sidecar.RevocationRecord{
		NetworkId: sidecar.Id(networkId),
		Kind:      kind,
		SubjectId: sidecar.Id(subjectId),
		NotBefore: notBefore.Truncate(time.Second),
		Reason:    sidecar.RevocationReasonRemoved,
		Time:      server.NowUtc().Truncate(time.Second),
	}
	payload, err := sidecar.EncodeRevocationPayload(record)
	if err != nil {
		t.Fatal(err)
	}
	object, err := oauth.SignRevocation(payload)
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.PutIfNoneMatch(context.Background(), record.Key(), object, nil); err != nil {
		t.Fatal(err)
	}
}

// One GET /socket/list with token: its status and, for a 200, its body.
func serviceTokenTestList(t testing.TB, host *Host, token string) (int, *sidecar.SocketListResult) {
	t.Helper()
	recorder := hostTestServe(host, http.MethodGet, "/socket/list", token, nil, "")
	if recorder.Code != http.StatusOK {
		return recorder.Code, nil
	}
	result := &sidecar.SocketListResult{}
	if err := json.Unmarshal(recorder.Body.Bytes(), result); err != nil {
		t.Fatal(err)
	}
	return recorder.Code, result
}

// A real block, its Authenticator the block's own: a sockets token with its sidecar is answered; an evs token and
// an expired sockets token answer 401; a token whose client, sidecar or member a rev/ object voids answers 401 once
// the rev/ reader's answer for that subject, kept 5 s by the block's clock, has lapsed.
func TestHostTakesSocketsServiceTokens(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		verifier := installServiceTokenTestKeys(t)
		world := newHostTestWorld(t)
		blobs := sidecartest.NewFakeBlobs(world.clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
		host := world.newHost(t, func(deps *HostDeps) {
			deps.Authenticator = nil
			deps.Blobs = blobs
		})

		caller := mintServiceTokenTestCaller(t, "sockets")
		code, result := serviceTokenTestList(t, host, caller.token)
		if code != http.StatusOK || result.Error != nil {
			t.Fatalf("a sockets token with its sidecar answered %d, %+v", code, result)
		}

		evsCaller := mintServiceTokenTestCaller(t, "evs")
		if code, result := serviceTokenTestList(t, host, evsCaller.token); code != http.StatusUnauthorized {
			t.Fatalf("an evs token answered %d, %+v", code, result)
		}

		verifier.Testing_SetNow(func() time.Time { return caller.expiry.Add(time.Second) })
		if code, result := serviceTokenTestList(t, host, caller.token); code != http.StatusUnauthorized {
			t.Fatalf("an expired sockets token answered %d, %+v", code, result)
		}
		verifier.Testing_SetNow(server.NowUtc)

		for _, kind := range []sidecar.RevocationKind{sidecar.RevocationKindClient, sidecar.RevocationKindSidecar, sidecar.RevocationKindMember} {
			caller := mintServiceTokenTestCaller(t, "sockets")
			if code, result := serviceTokenTestList(t, host, caller.token); code != http.StatusOK || result.Error != nil {
				t.Fatalf("%s: before its rev/ object, %d, %+v", kind, code, result)
			}
			subjectId := map[sidecar.RevocationKind]server.Id{
				sidecar.RevocationKindClient:  caller.clientId,
				sidecar.RevocationKindSidecar: caller.sidecarId,
				sidecar.RevocationKindMember:  caller.memberId,
			}[kind]
			revokeServiceTokenTestSubject(t, blobs, caller.networkId, kind, subjectId, server.NowUtc().Add(time.Minute))
			world.clock.Advance(DefaultHostSettings().Revocation.CacheTimeout)
			if code, result := serviceTokenTestList(t, host, caller.token); code != http.StatusUnauthorized {
				t.Fatalf("a token whose %s a rev/ object voids answered %d, %+v", kind, code, result)
			}
		}
	})
}

// A block with no first metadata cluster has no rev/ reader: it says so once at start, and every authenticated route
// answers unavailable, never 401 and never an unchecked allow.
func TestHostWithNoClusterAnswersUnavailable(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		installServiceTokenTestKeys(t)
		world := newHostTestWorld(t)
		host := world.newHost(t, func(deps *HostDeps) {
			deps.Authenticator = nil
		})
		caller := mintServiceTokenTestCaller(t, "sockets")
		code, result := serviceTokenTestList(t, host, caller.token)
		if code != http.StatusOK || result.Error == nil || result.Error.Code != sidecar.SocketErrorCodeUnavailable {
			t.Fatalf("a block with no rev/ reader answered %d, %+v", code, result)
		}
		said := false
		for _, line := range world.log.Lines() {
			if strings.Contains(line, "no rev/ reader") {
				said = true
			}
		}
		if !said {
			t.Fatalf("the block never said it has no rev/ reader: %q", world.log.Lines())
		}
	})
}
