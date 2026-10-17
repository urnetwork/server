// Signature-Agent is caller-controlled classification, never quota authority.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Observes completed real Redis increments, scoped to this fixture's requests.
type lagSignatureQuotaProbe struct {
	client    atomic.Int64
	prefix    atomic.Int64
	signature atomic.Int64
	guest     atomic.Int64
}

// Distinguishes request observations from unrelated shared-pool commands.
type lagSignatureQuotaContextKey struct{}

// Adds a read-only command tripwire; no Redis result or key is replaced.
type lagSignatureQuotaHook struct {
	probe *lagSignatureQuotaProbe
}

// Leaves actual fixture connections unchanged.
func (self lagSignatureQuotaHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

// Observes successful single-command execution without changing its result.
func (self lagSignatureQuotaHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err == nil {
			self.observe(ctx, cmd)
		}
		return err
	}
}

// Includes the transaction pipeline used by the actual lag rate limiter.
func (self lagSignatureQuotaHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		err := next(ctx, commands)
		if err == nil {
			for _, cmd := range commands {
				self.observe(ctx, cmd)
			}
		}
		return err
	}
}

// Counts only this request context's actual quota increments, not key reads.
func (self lagSignatureQuotaHook) observe(ctx context.Context, cmd redis.Cmder) {
	if ctx.Value(lagSignatureQuotaContextKey{}) != self.probe || cmd.Name() != "incr" || len(cmd.Args()) != 2 {
		return
	}
	key, ok := cmd.Args()[1].(string)
	if !ok {
		return
	}
	switch {
	case strings.HasPrefix(key, "{lag_track_client_"):
		self.probe.client.Add(1)
	case strings.HasPrefix(key, "{lag_track_prefix_"):
		self.probe.prefix.Add(1)
	case strings.HasPrefix(key, "{lag_track_signature_"):
		self.probe.signature.Add(1)
	case strings.HasPrefix(key, "{lag_session_prefix_"):
		self.probe.guest.Add(1)
	}
}

// Installs a context-bound observer and settings in the existing isolated DBs.
func lagSignatureAdmissionFixture(t testing.TB, settings *LagSettings) (context.Context, *lagSignatureQuotaProbe) {
	t.Helper()
	// These synthetic windows do not roll over at an hour boundary during
	// admission assertions. Production defaults and clock behavior are unchanged.
	settings.TrackLimitWindow = 100 * 365 * 24 * time.Hour
	settings.GuestLimitWindow = settings.TrackLimitWindow
	t.Cleanup(Testing_SetLagSettings(settings))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	probe := &lagSignatureQuotaProbe{}
	ctx = context.WithValue(ctx, lagSignatureQuotaContextKey{}, probe)
	server.Redis(ctx, func(client server.RedisClient) {
		client.AddHook(lagSignatureQuotaHook{probe: probe})
	})
	installLagTestExtenderConfig(t, true)
	useLagAddressPlaces(t, map[string]*model.LagPlace{})
	useIpAsnDb(t, "")
	return ctx, probe
}

// Creates a database-backed normal client and a genuinely signed client JWT.
func lagSignatureAuthenticatedClient(t testing.TB, ctx context.Context) (string, *session.ByJwt) {
	t.Helper()
	networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "lag-signature-admission", userId)
	model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "synthetic client", "test")
	byJwt := session.NewByJwt(networkId, userId, "lag-signature-admission", false, false).Client(deviceId, clientId)
	return byJwt.Testing_Sign(), byJwt
}

// Uses the exact authentication/controller wrapper of POST /lag/track, with
// no listener and no pretrusted ClientSession bypassing JWT verification.
func lagSignatureTrackRequest(t testing.TB, ctx context.Context, token, address string, header http.Header, args *LagTrackArgs) (int, *LagTrackResult) {
	t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "https://lag.example/lag/track", bytes.NewReader(body)).WithContext(ctx)
	req.RemoteAddr = address
	req.Header = header.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.WrapWithInputRequireClient(LagTrack, response, req)
	if response.Code != http.StatusOK {
		return response.Code, nil
	}
	result := &LagTrackResult{}
	if err := json.Unmarshal(response.Body.Bytes(), result); err != nil {
		t.Fatalf("decode track response: %v", err)
	}
	return response.Code, result
}

// Uses the actual unauthenticated guest-session wrapper and durable creation.
func lagSignatureGuestRequest(t testing.TB, ctx context.Context, address string, header http.Header) *LagSessionResult {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "https://lag.example/lag/session", nil).WithContext(ctx)
	req.RemoteAddr = address
	req.Header = header.Clone()
	response := httptest.NewRecorder()
	router.WrapNoAuth(LagSession, response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("guest handler status=%d", response.Code)
	}
	result := &LagSessionResult{}
	if err := json.Unmarshal(response.Body.Bytes(), result); err != nil {
		t.Fatalf("decode guest response: %v", err)
	}
	return result
}

// Anonymous callers cannot exhaust an agent-named global bucket through real
// guest credentials, even when no requested extender can issue a ticket.
func TestLagTrackSignatureClaimCannotDebitAgentQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := DefaultLagSettings()
		settings.TrackSignatureLimit = 2
		ctx, probe := lagSignatureAdmissionFixture(t, settings)
		rootPublicKey := installLagTestExtenderConfig(t, true)
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		extender := newLagServingTestExtender(t, ctx, places, lagTestPlace("Example", "zz", 1, 2), "192.0.2.1")
		header := http.Header{"Signature-Agent": {`"https://agent.example"`}}
		guest := lagSignatureGuestRequest(t, ctx, "198.51.100.10:1000", header)
		if guest.Error != nil || guest.ByClientJwt == "" || guest.ClientId == nil {
			t.Fatal("fixture guest authentication was not issued")
		}
		missing := &LagTrackArgs{Extenders: []*LagTrackExtenderArgs{nil}}
		for range settings.TrackSignatureLimit {
			status, result := lagSignatureTrackRequest(t, ctx, guest.ByClientJwt, "198.51.100.10:1000", header, missing)
			if status != http.StatusOK || result.Error == nil || result.Error.Code != LagErrorCodeUnavailable {
				t.Fatalf("authenticated guest attempt did not reach extender admission: status=%d result=%+v", status, result)
			}
		}
		token, client := lagSignatureAuthenticatedClient(t, ctx)
		valid := &LagTrackArgs{Extenders: []*LagTrackExtenderArgs{{ExtenderId: extender.extenderId}}}
		status, result := lagSignatureTrackRequest(t, ctx, token, "203.0.113.10:1000", header, valid)
		if probe.client.Load() != 3 || probe.prefix.Load() != 3 {
			t.Fatalf("actual caller quotas not observed: client=%d prefix=%d", probe.client.Load(), probe.prefix.Load())
		}
		if got := probe.signature.Load(); got != 0 {
			code := "none"
			if result != nil && result.Error != nil {
				code = result.Error.Code
			}
			t.Fatalf("unverified header spent global agent quota: increments=%d want=0; independent signed client result=%s", got, code)
		}
		if status != http.StatusOK || result.Error != nil || result.TrackId == nil || len(result.Tickets) != 1 {
			t.Fatalf("independent signed client was not admitted: status=%d result=%+v", status, result)
		}
		body := verifyLagTestTicket(t, result.Tickets[0].Ticket, rootPublicKey, server.NowUtc())
		track := model.GetLagTrack(ctx, *result.TrackId)
		if !bytes.Equal(body.ClientId, client.ClientId.Bytes()) || track.ClientId != *client.ClientId || track.NetworkId != client.NetworkId {
			t.Fatal("claimed agent replaced the authenticated ticket or stored track identity")
		}
		if track.AutomatedReason != model.LagAutomatedReasonSignatureAgent {
			t.Fatal("untrusted claim lost conservative automation classification")
		}
		status, result = lagSignatureTrackRequest(t, ctx, guest.ByClientJwt, "198.51.100.10:1000", header, valid)
		if status != http.StatusOK || result.Error != nil || len(result.Tickets) != 1 {
			t.Fatalf("signed guest lost ticket admission: status=%d result=%+v", status, result)
		}
		body = verifyLagTestTicket(t, result.Tickets[0].Ticket, rootPublicKey, server.NowUtc())
		if !bytes.Equal(body.ClientId, guest.ClientId.Bytes()) || probe.signature.Load() != 0 {
			t.Fatal("guest ticket identity or quota authority changed")
		}
	})
}

// Malformed, forged and replayed agent claims authenticate nobody. A real
// signed client still spends its own quota for each replay; no bot-signature
// verifier is implied by accepting a Signature or Signature-Input header.
func TestLagTrackSignatureClaimsAreNotAuthentication(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, probe := lagSignatureAdmissionFixture(t, DefaultLagSettings())
		token, client := lagSignatureAuthenticatedClient(t, ctx)
		args := &LagTrackArgs{Extenders: []*LagTrackExtenderArgs{nil}}
		key, err := apikey.Testing_CreateApiKey(client.NetworkId, ctx)
		if err != nil {
			t.Fatal(err)
		}
		// A genuine network API key still lacks the required client identity.
		for _, invalid := range []string{"", "invalid.jwt.signature", token + "x", key.ApiKey} {
			status, _ := lagSignatureTrackRequest(t, ctx, invalid, "192.0.2.10:1000", http.Header{"Signature-Agent": {`"https://agent.example"`}}, args)
			if status != http.StatusUnauthorized {
				t.Fatalf("an agent claim authenticated an invalid caller: status=%d", status)
			}
		}
		if probe.client.Load() != 0 || probe.prefix.Load() != 0 || probe.signature.Load() != 0 {
			t.Fatal("unauthenticated requests reached lag quota admission")
		}
		for _, header := range []http.Header{
			nil,
			{"Signature-Agent": {"   "}},
			{"Signature-Agent": {"not-a-structured-agent"}},
			{"Signature-Agent": {`"https://agent.example"`}},
			{"Signature-Agent": {`"https://agent.example"`, `"https://other.example"`}},
			{"Signature-Agent": {`"https://agent.example"`}, "Signature-Input": {`sig=("@method");keyid="synthetic"`}, "Signature": {"sig=:AA==:"}},
			{"Signature-Agent": {`"https://agent.example"`}, "Signature-Input": {`sig=("@method");keyid="synthetic"`}, "Signature": {"sig=:AA==:"}},
		} {
			status, result := lagSignatureTrackRequest(t, ctx, token, "192.0.2.10:1000", header, args)
			if status != http.StatusOK || result.Error == nil || result.Error.Code != LagErrorCodeUnavailable {
				t.Fatalf("valid client did not retain ordinary admission: status=%d result=%+v", status, result)
			}
		}
		if probe.client.Load() != 7 || probe.prefix.Load() != 7 || probe.signature.Load() != 0 {
			t.Fatalf("header changed quota authority: client=%d prefix=%d agent=%d", probe.client.Load(), probe.prefix.Load(), probe.signature.Load())
		}
	})
}

// Changing, replaying or removing an agent claim cannot bypass either the
// authenticated client quota or the shared caller-prefix quota.
func TestLagTrackSignatureClaimPreservesCallerLimits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := DefaultLagSettings()
		settings.TrackClientLimit, settings.TrackPrefixLimit, settings.TrackSignatureLimit = 2, 3, 1
		ctx, probe := lagSignatureAdmissionFixture(t, settings)
		first := lagSignatureGuestRequest(t, ctx, "198.51.100.10:1000", nil)
		second := lagSignatureGuestRequest(t, ctx, "198.51.100.11:1000", nil)
		if first.Error != nil || second.Error != nil || first.ByClientJwt == "" || second.ByClientJwt == "" {
			t.Fatal("fixture guest credentials were not issued")
		}
		other, _ := lagSignatureAuthenticatedClient(t, ctx)
		for i, row := range []struct {
			token   string
			address string
			agent   string
			want    string
		}{
			{token: first.ByClientJwt, address: "198.51.100.10:1000", agent: `"https://agent.example"`, want: LagErrorCodeUnavailable},
			{token: first.ByClientJwt, address: "198.51.100.10:1000", agent: "malformed", want: LagErrorCodeUnavailable},
			{token: first.ByClientJwt, address: "198.51.100.10:1000", agent: "", want: LagErrorCodeLimit},
			{token: second.ByClientJwt, address: "198.51.100.11:1000", agent: `"https://agent.example"`, want: LagErrorCodeUnavailable},
			{token: second.ByClientJwt, address: "198.51.100.11:1000", agent: `"https://other.example"`, want: LagErrorCodeLimit},
			{token: other, address: "203.0.113.10:1000", agent: "", want: LagErrorCodeUnavailable},
		} {
			status, result := lagSignatureTrackRequest(t, ctx, row.token, row.address, http.Header{"Signature-Agent": {row.agent}}, &LagTrackArgs{Extenders: []*LagTrackExtenderArgs{nil}})
			if status != http.StatusOK || result.Error == nil || result.Error.Code != row.want {
				t.Fatalf("caller quota row %d: status=%d result=%+v want=%s", i, status, result, row.want)
			}
			if row.want == LagErrorCodeLimit && result.Error.RetryAfterMs <= 0 {
				t.Fatalf("caller quota row %d omitted retry bound", i)
			}
		}
		if probe.client.Load() != 6 || probe.prefix.Load() != 5 || probe.signature.Load() != 0 {
			t.Fatalf("caller quota ordering changed: client=%d prefix=%d agent=%d", probe.client.Load(), probe.prefix.Load(), probe.signature.Load())
		}
	})
}

// The unauthenticated session endpoint still limits actual guest creation by
// caller prefix, regardless of a claimed agent name or forged signature.
func TestLagTrackSignatureClaimPreservesGuestLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := DefaultLagSettings()
		settings.GuestLimit = 2
		ctx, probe := lagSignatureAdmissionFixture(t, settings)
		for i := range 3 {
			header := http.Header{"Signature-Agent": {fmt.Sprintf(`"https://agent-%d.example"`, i)}, "Signature": {"sig=:AA==:"}}
			result := lagSignatureGuestRequest(t, ctx, fmt.Sprintf("198.51.100.%d:1000", 10+i), header)
			if i < 2 {
				if result.Error != nil || result.ByClientJwt == "" {
					t.Fatal("guest below its prefix limit was refused")
				}
			} else if result.Error == nil || result.Error.Code != LagErrorCodeLimit || result.ByClientJwt != "" || result.NetworkId != nil || result.Error.RetryAfterMs <= 0 {
				t.Fatal("changing the agent claim bypassed the guest prefix limit")
			}
		}
		if result := lagSignatureGuestRequest(t, ctx, "203.0.113.10:1000", nil); result.Error != nil || result.ByClientJwt == "" {
			t.Fatal("independent guest prefix was not admitted")
		}
		if probe.guest.Load() != 4 || probe.signature.Load() != 0 || probe.client.Load() != 0 || probe.prefix.Load() != 0 {
			t.Fatalf("guest quota authority changed: guests=%d agent=%d client=%d prefix=%d", probe.guest.Load(), probe.signature.Load(), probe.client.Load(), probe.prefix.Load())
		}
	})
}
