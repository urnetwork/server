// Source-only authored controls use real payoutartifact Build/Sign/Decode and
// the public Signal boundary. All transport observations are inert fixtures.
package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/payoutartifact"
)

// The actual optional Do seam counts every attempted read, including failures.
type subnetArtifactTestSource struct {
	*syntheticSource
	do    func(*http.Request) (*http.Response, error)
	calls int
}

// No default test path reaches a socket, shell or production source.
func (self *subnetArtifactTestSource) Do(request *http.Request) (*http.Response, error) {
	self.calls++
	return self.do(request)
}

// The fixed public test key signs synthetic provider observations only.
func signSubnetArtifactTest(t *testing.T, artifact *payoutartifact.Artifact) {
	t.Helper()
	key, err := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := payoutartifact.Sign(artifact, key); err != nil {
		t.Fatal(err)
	}
}

// Independent expected values are taken from the fixture before an adversarial
// response is changed. This helper does not claim production enrollment exists.
func subnetArtifactFixture(t *testing.T) (SignalSettings, *subnetArtifactTestSource, *payoutartifact.Artifact, []byte) {
	t.Helper()
	artifact, err := payoutartifact.Build(payoutartifact.BuildInput{
		DeploymentID: "private-synthetic-deployment", ChainID: 945, Netuid: 521,
		GenesisHash: "0x" + strings.Repeat("ab", 32), PolicyHash: "0x" + strings.Repeat("cd", 32),
		Coordinator: common.HexToAddress("0x100"), SettlementVault: common.HexToAddress("0x200"),
		Epoch: 4, NoID: 1,
		Start:                payoutartifact.Boundary{Number: 100, Hash: "0x" + strings.Repeat("01", 32)},
		End:                  payoutartifact.Boundary{Number: 200, Hash: "0x" + strings.Repeat("02", 32)},
		OperatorSnapshotHash: "sha256:" + strings.Repeat("10", 32),
		FleetSnapshotHash:    "sha256:" + strings.Repeat("20", 32),
		Providers: []payoutartifact.ProviderInput{
			{ClientID: [16]byte{1}, NetworkID: [16]byte{11}, Coldkey: [32]byte{1}, UsageBytes: 100, Assignments: 10, Confirmations: 10, Eligible: true},
			{ClientID: [16]byte{2}, NetworkID: [16]byte{12}, Coldkey: [32]byte{2}, UsageBytes: 100, Assignments: 10, Confirmations: 5, Eligible: true},
		},
		ReliabilityAMin: 8, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	signSubnetArtifactTest(t, artifact)
	raw, err := payoutartifact.Bytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	wireHash := sha256.Sum256(raw)
	source := &subnetArtifactTestSource{syntheticSource: &syntheticSource{}}
	settings := syntheticSettings(source)
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-subnet-artifact-fixture"
	settings.VerificationEnabled = true
	settings.SubnetArtifacts = &SubnetArtifactSettings{
		Schema:      subnetArtifactEnrollmentSchema,
		ReviewFrom:  settings.Now().Add(-time.Hour).Format(time.RFC3339Nano),
		ReviewUntil: settings.Now().Add(time.Hour).Format(time.RFC3339Nano),
		Origins:     []string{"https://first.private.example.test", "https://second.private.example.test"},
		Objects: []SubnetArtifactExpectation{{
			ContentHash: artifact.ContentHash, WireSha256: hex.EncodeToString(wireHash[:]),
			DeploymentId: artifact.DeploymentID, ChainId: artifact.ChainID, GenesisHash: artifact.GenesisHash, Netuid: artifact.Netuid,
			Coordinator: strings.ToLower(artifact.Coordinator.Hex()), SettlementVault: strings.ToLower(artifact.SettlementVault.Hex()),
			Epoch: artifact.Epoch, NoId: artifact.NoID, PolicyHash: artifact.PolicyHash, Signer: strings.ToLower(artifact.Signer.Hex()),
			StartNumber: artifact.Start.Number, StartHash: artifact.Start.Hash, EndNumber: artifact.End.Number, EndHash: artifact.End.Hash,
			PayoutRoot: "0x" + hex.EncodeToString(artifact.PayoutRoot[:]),
		}},
	}
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return true, nil }
	source.do = func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/sn/artifact" || request.URL.Query().Get("hash") != artifact.ContentHash || len(request.URL.Query()) != 1 || request.Header.Get("Authorization") != "" || request.Header.Get("Accept-Encoding") != "identity" {
			t.Fatal("request departed from the real public artifact contract")
		}
		return subnetArtifactTestResponse(request, raw, artifact.ContentHash), nil
	}
	if err := settings.Validate(); err != nil {
		t.Fatalf("subnet artifact fixture settings are invalid before source entry: %v", err)
	}
	return settings, source, artifact, raw
}

// A response models the exact existing handler headers and canonical body.
func subnetArtifactTestResponse(request *http.Request, raw []byte, hash string) *http.Response {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("ETag", `"`+hash+`"`)
	return &http.Response{StatusCode: http.StatusOK, Header: header, ContentLength: int64(len(raw)), Body: io.NopCloser(bytes.NewReader(raw)), Request: request}
}

// Narrow successful reads do not alter the full nine-family coverage sentinel.
func TestSubnetArtifactsReadsOriginalSignedObjectsAtBothOrigins(t *testing.T) {
	settings, source, artifact, _ := subnetArtifactFixture(t)
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || source.calls != 2 {
		t.Fatal("two-origin original artifact control did not pass", err, len(alerts), source.calls)
	}
	if artifact.CreatedAt >= settings.SubnetArtifacts.ReviewFrom {
		t.Fatal("fixture stopped exercising old historical content")
	}
	coverage, err := NewSubnetCoverageSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, coverage, "subnet-monitor-coverage")
	if !strings.Contains(alert.Observed, "implemented_correctness_families=0") {
		t.Fatal("partial artifact reads claimed a full family")
	}
}

// An origin outage cannot be hidden by its healthy peer or a successful status
// endpoint; only the exact enrolled public object route is contacted.
func TestSubnetArtifactsRetainsPartialOriginLoss(t *testing.T) {
	settings, source, artifact, raw := subnetArtifactFixture(t)
	source.do = func(request *http.Request) (*http.Response, error) {
		response := subnetArtifactTestResponse(request, raw, artifact.ContentHash)
		if source.calls == 1 {
			response.StatusCode = http.StatusNotFound
		}
		return response, nil
	}
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || source.calls != 2 {
		t.Fatal("partial loss was concealed", err, len(alerts), source.calls)
	}
	alert := requireAlertClass(t, alerts, "subnet-history-availability")
	if alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.PageSustain != 0 || !alert.unobservable || alert.Target != "subnet-artifacts/object-1/origin-1" {
		t.Fatal("availability changed its scope or policy")
	}
	requireAlertOmits(t, alert, settings.SubnetArtifacts.Origins[0], artifact.ContentHash, artifact.DeploymentID, artifact.Signer.Hex())
}

// Unreachable API fronts do not prove that every backing copy or earned claim
// is lost, so no guessed PAGE threshold or incident disposition is introduced.
func TestSubnetArtifactsAllOriginsUnavailableRemainUnknown(t *testing.T) {
	settings, source, _, _ := subnetArtifactFixture(t)
	source.do = func(*http.Request) (*http.Response, error) { return nil, errors.New("private-transport-token") }
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 || source.calls != 2 {
		t.Fatal("all-origin gap vanished")
	}
	for _, alert := range alerts {
		if alert.Severity != SeverityWarn || !alert.unobservable || alert.PageSustain != 0 {
			t.Fatal("unknown backing loss was promoted to financial proof")
		}
		requireAlertOmits(t, alert, "private-transport-token")
	}
}

// A valid signer and content hash cannot authorize foreign domains or make a
// false payout summary reconstruct. Expected wire pins alone are insufficient.
func TestSubnetArtifactsRejectsResignedMaliciousObjects(t *testing.T) {
	for _, item := range []struct {
		name   string
		change func(*payoutartifact.Artifact)
	}{
		{name: "foreign deployment", change: func(a *payoutartifact.Artifact) { a.DeploymentID = "foreign-private-deployment" }},
		{name: "foreign chain", change: func(a *payoutartifact.Artifact) { a.ChainID++ }},
		{name: "foreign operator", change: func(a *payoutartifact.Artifact) { a.NoID++ }},
		{name: "foreign epoch", change: func(a *payoutartifact.Artifact) { a.Epoch++ }},
		{name: "foreign boundary", change: func(a *payoutartifact.Artifact) { a.End.Number++ }},
		{name: "false usage", change: func(a *payoutartifact.Artifact) { a.TotalUsageBytes++ }},
		{name: "false payout", change: func(a *payoutartifact.Artifact) { a.Leaves[0].ShareBPS++ }},
		{name: "false root", change: func(a *payoutartifact.Artifact) { a.PayoutRoot[0] ^= 1 }},
	} {
		settings, source, artifact, _ := subnetArtifactFixture(t)
		item.change(artifact)
		signSubnetArtifactTest(t, artifact)
		raw, err := json.Marshal(artifact)
		if err != nil {
			t.Fatal(err)
		}
		wireHash := sha256.Sum256(raw)
		settings.SubnetArtifacts.Objects[0].ContentHash = artifact.ContentHash
		settings.SubnetArtifacts.Objects[0].WireSha256 = hex.EncodeToString(wireHash[:])
		source.do = func(request *http.Request) (*http.Response, error) {
			return subnetArtifactTestResponse(request, raw, artifact.ContentHash), nil
		}
		alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 2 || source.calls != 2 {
			t.Fatalf("%s: malicious object appeared available", item.name)
		}
		for _, alert := range alerts {
			if !alert.unobservable {
				t.Fatalf("%s: asserted accepted tamper without inclusion evidence", item.name)
			}
		}
	}
}

// The protocol content hash excludes the signer; the independent signer pin
// therefore has a separate adversarial control using otherwise identical content.
func TestSubnetArtifactsDoesNotLearnSignerFromResponse(t *testing.T) {
	settings, source, artifact, _ := subnetArtifactFixture(t)
	key, err := crypto.HexToECDSA("1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := payoutartifact.Sign(artifact, key); err != nil {
		t.Fatal(err)
	}
	raw, err := payoutartifact.Bytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	wireHash := sha256.Sum256(raw)
	settings.SubnetArtifacts.Objects[0].WireSha256 = hex.EncodeToString(wireHash[:])
	source.do = func(request *http.Request) (*http.Response, error) {
		return subnetArtifactTestResponse(request, raw, artifact.ContentHash), nil
	}
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 || !strings.Contains(alerts[0].Observed, "artifact-expectation-mismatch") {
		t.Fatal("response selected its own signer")
	}
}

// Exact body bounds precede decoding; canonical JSON disallows alternate
// spellings, duplicates, trailers, unknown fields and a misleading healthy body.
func TestSubnetArtifactsRejectsBadWireEncoding(t *testing.T) {
	for _, item := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{name: "healthy assertion", change: func([]byte) []byte { return []byte(`{"healthy":true}`) }},
		{name: "unknown field", change: func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"schema":`), []byte(`"unknown":1,"schema":`), 1)
		}},
		{name: "duplicate field", change: func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"schema":`), []byte(`"schema":"urnetwork-payout-artifact-v1","schema":`), 1)
		}},
		{name: "trailing value", change: func(b []byte) []byte { return append(b, []byte(`{}`)...) }},
		{name: "alternate whitespace", change: func(b []byte) []byte { return append(b, '\n') }},
		{name: "truncated", change: func(b []byte) []byte { return b[:len(b)-1] }},
		{name: "non-json", change: func([]byte) []byte { return []byte("private-error-page") }},
	} {
		settings, source, artifact, original := subnetArtifactFixture(t)
		raw := item.change(append([]byte(nil), original...))
		wireHash := sha256.Sum256(raw)
		settings.SubnetArtifacts.Objects[0].WireSha256 = hex.EncodeToString(wireHash[:])
		source.do = func(request *http.Request) (*http.Response, error) {
			return subnetArtifactTestResponse(request, raw, artifact.ContentHash), nil
		}
		alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 2 {
			t.Fatalf("%s: bad encoding appeared available", item.name)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-error-page")
		}
	}
}

// Ordinary HTTP success is insufficient if framing, encoding, identity or
// integrity differs from the exact existing immutable-object contract.
func TestSubnetArtifactsRejectsMisleadingHttpSuccess(t *testing.T) {
	for _, item := range []struct {
		name   string
		change func(*http.Response)
	}{
		{name: "empty body", change: func(r *http.Response) { r.Body = nil }},
		{name: "wrong media", change: func(r *http.Response) { r.Header.Set("Content-Type", "text/html") }},
		{name: "encoded", change: func(r *http.Response) { r.Header.Set("Content-Encoding", "gzip") }},
		{name: "decompressed", change: func(r *http.Response) { r.Uncompressed = true }},
		{name: "missing tag", change: func(r *http.Response) { r.Header.Del("ETag") }},
		{name: "partial status", change: func(r *http.Response) { r.StatusCode = http.StatusPartialContent }},
		{name: "wrong length", change: func(r *http.Response) { r.ContentLength++ }},
		{name: "oversized declaration", change: func(r *http.Response) { r.ContentLength = maximumSubnetArtifactBytes + 1 }},
	} {
		settings, source, artifact, raw := subnetArtifactFixture(t)
		source.do = func(request *http.Request) (*http.Response, error) {
			response := subnetArtifactTestResponse(request, raw, artifact.ContentHash)
			item.change(response)
			return response, nil
		}
		alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 2 {
			t.Fatalf("%s: misleading HTTP response passed", item.name)
		}
	}
}

// Body read/close failures are real source boundaries, not successful delivery.
type subnetArtifactTestBody struct {
	reader    io.Reader
	readErr   error
	closeErr  error
	readBytes int
	closes    int
}

// The exact byte count proves the cap before the artifact decoder is entered.
func (self *subnetArtifactTestBody) Read(buffer []byte) (int, error) {
	n, err := self.reader.Read(buffer)
	self.readBytes += n
	if self.readErr != nil && n > 0 {
		return n, self.readErr
	}
	return n, err
}

// Closing is counted even when the owned body reports a source failure.
func (self *subnetArtifactTestBody) Close() error { self.closes++; return self.closeErr }

// An unknown content length cannot bypass the pre-allocation byte admission.
func TestSubnetArtifactsBoundsActualBodyAndCloses(t *testing.T) {
	settings, source, artifact, _ := subnetArtifactFixture(t)
	bodies := []*subnetArtifactTestBody{}
	source.do = func(request *http.Request) (*http.Response, error) {
		body := &subnetArtifactTestBody{reader: bytes.NewReader(bytes.Repeat([]byte("x"), maximumSubnetArtifactBytes+100))}
		bodies = append(bodies, body)
		response := subnetArtifactTestResponse(request, nil, artifact.ContentHash)
		response.Body, response.ContentLength = body, -1
		return response, nil
	}
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 || len(bodies) != 2 {
		t.Fatal("oversized actual body was accepted")
	}
	for _, body := range bodies {
		if body.readBytes != maximumSubnetArtifactBytes+1 || body.closes != 1 {
			t.Fatal("body escaped its exact read or close owner")
		}
	}
}

// Complete bytes still cannot hide either an ordinary read or close error.
func TestSubnetArtifactsReadAndCloseErrorsStayUnknown(t *testing.T) {
	for _, closeFailure := range []bool{false, true} {
		settings, source, artifact, raw := subnetArtifactFixture(t)
		bodies := []*subnetArtifactTestBody{}
		source.do = func(request *http.Request) (*http.Response, error) {
			body := &subnetArtifactTestBody{reader: bytes.NewReader(raw)}
			if closeFailure {
				body.closeErr = errors.New("private-close-token")
			} else {
				body.readErr = errors.New("private-read-token")
			}
			bodies = append(bodies, body)
			response := subnetArtifactTestResponse(request, raw, artifact.ContentHash)
			response.Body = body
			return response, nil
		}
		alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 2 {
			t.Fatal("source body failure became success")
		}
		for _, body := range bodies {
			if body.closes != 1 {
				t.Fatal("body close owner lost")
			}
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-close-token", "private-read-token")
		}
	}
}

// Is/As preserve the exact private leaf, while public error text remains safe.
type subnetArtifactPrivateCause struct{}

// This string must never be rendered into an alert or top-level run error.
func (*subnetArtifactPrivateCause) Error() string { return "private-artifact-transport-token" }

// Live-parent source cancellation at Do, Read or Close stops before origin two.
func TestSubnetArtifactsPreservesWrappedSourceCancellation(t *testing.T) {
	for _, stage := range []string{"do", "read", "close"} {
		for _, scoped := range []bool{false, true} {
			settings, source, artifact, raw := subnetArtifactFixture(t)
			if scoped {
				settings.ExcludedHosts = []string{"pg-1"}
			}
			leaf := &subnetArtifactPrivateCause{}
			cause := fmt.Errorf("private-wrapper: %w", errors.Join(context.Canceled, leaf))
			source.do = func(request *http.Request) (*http.Response, error) {
				if stage == "do" {
					return nil, cause
				}
				body := &subnetArtifactTestBody{reader: bytes.NewReader(raw)}
				if stage == "read" {
					body.readErr = cause
				} else {
					body.closeErr = cause
				}
				response := subnetArtifactTestResponse(request, raw, artifact.ContentHash)
				response.Body = body
				return response, nil
			}
			alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
			var actual *subnetArtifactPrivateCause
			if !errors.Is(err, context.Canceled) || !errors.Is(err, leaf) || !errors.As(err, &actual) || actual != leaf || source.calls != 1 || len(alerts) != 0 {
				t.Fatalf("%s scoped=%v: source cancel was flattened or continued", stage, scoped)
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("source cancellation leaked private text")
			}
		}
	}
}

// This deterministic parent advances to DeadlineExceeded only from inside the
// admitted source callback. No setup time or scheduler delay can expire it first.
type subnetArtifactEntryDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

// The derived request context observes the same terminal channel as its parent.
func (self *subnetArtifactEntryDeadline) Done() <-chan struct{} { return self.done }

// The published terminal state is synchronized by closing the channel.
func (self *subnetArtifactEntryDeadline) Err() error {
	select {
	case <-self.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// Both test cleanup and source entry may safely request the one transition.
func (self *subnetArtifactEntryDeadline) expire() { self.once.Do(func() { close(self.done) }) }

// Real derived cancellation follows actual source entry for both parent modes.
func TestSubnetArtifactsOwnsParentCancellationAfterReadEntry(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		settings, source, _, _ := subnetArtifactFixture(t)
		var parent context.Context
		var stop func()
		want := error(context.Canceled)
		if deadline {
			controlled := &subnetArtifactEntryDeadline{Context: context.Background(), done: make(chan struct{})}
			parent, stop, want = controlled, controlled.expire, context.DeadlineExceeded
		} else {
			parent, stop = context.WithCancel(context.Background())
		}
		leaf := &subnetArtifactPrivateCause{}
		source.do = func(request *http.Request) (*http.Response, error) {
			if parent.Err() != nil || request.Context().Err() != nil {
				t.Fatal("parent expired before admitted entry")
			}
			stop()
			<-request.Context().Done()
			return nil, errors.Join(request.Context().Err(), leaf)
		}
		alerts, err := NewSubnetArtifactsSignal().Run(parent, settings)
		stop()
		var actual *subnetArtifactPrivateCause
		if !errors.Is(err, want) || !errors.Is(err, leaf) || !errors.As(err, &actual) || actual != leaf || source.calls != 1 || len(alerts) != 0 {
			t.Fatal("actual parent cancellation lost source cause or continued")
		}
	}
}

// An already canceled owner has no source entry; an actual child deadline is
// instead an unknown read, with its live parent left usable for a sibling.
func TestSubnetArtifactsDistinguishesParentAndChildDeadline(t *testing.T) {
	settings, source, _, _ := subnetArtifactFixture(t)
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSubnetArtifactsSignal().Run(parent, settings); !errors.Is(err, context.Canceled) || source.calls != 0 {
		t.Fatal("canceled parent contacted source")
	}
	source.do = func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}
	reason, err, readOwnerErr := readSubnetArtifact(context.Background(), source, settings.SubnetArtifacts.Origins[0], settings.SubnetArtifacts.Objects[0], time.Millisecond)
	if reason == "" || !errors.Is(err, context.DeadlineExceeded) || readOwnerErr != context.DeadlineExceeded || subnetArtifactCancellation(context.Background(), err, readOwnerErr) != nil || source.calls != 1 {
		t.Fatal("owned child deadline was promoted to parent cancellation")
	}
}

// Settings transitions during a successful body read revoke its authority and
// stop before the next origin; no successful stale prefix can clear a gap.
func TestSubnetArtifactsRejectsStaleAuthorityAtCompletion(t *testing.T) {
	for _, mode := range []string{"stale", "expiry", "clock"} {
		settings, source, artifact, raw := subnetArtifactFixture(t)
		now := settings.Now()
		initial := now
		settings.Now = func() time.Time { return now }
		current := true
		settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return current, nil }
		source.do = func(request *http.Request) (*http.Response, error) {
			switch mode {
			case "stale":
				current = false
			case "expiry":
				now = initial.Add(2 * time.Hour)
			case "clock":
				now = initial.Add(-time.Minute)
			}
			return subnetArtifactTestResponse(request, raw, artifact.ContentHash), nil
		}
		alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
		if err != nil || source.calls != 1 || len(alerts) != 1 || alerts[0].Target != "subnet-artifacts/configuration" || !alerts[0].unobservable {
			t.Fatalf("%s: completion authority changed silently", mode)
		}
	}
}

// Host exclusions guard this new native/injected HTTP path before Do without
// pretending that the explicitly withheld origin was successfully verified.
func TestSubnetArtifactsHonorsHostScopeBeforeContact(t *testing.T) {
	settings, source, _, _ := subnetArtifactFixture(t)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "first.private.example.test", Roles: []string{"api"}})
	settings.ExcludedHosts = []string{"first.private.example.test"}
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || source.calls != 1 {
		t.Fatal("excluded artifact origin was contacted or healthy sibling skipped")
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	alert := requireAlertClass(t, alerts, "subnet-history-availability")
	if !alert.unobservable {
		t.Fatal("scope denial became successful artifact verification")
	}
}

// A structural native-client control and its real redirect handling require no
// network. The injected RoundTripper supplies an actual HTTP redirect response.
type subnetArtifactRoundTripper func(*http.Request) (*http.Response, error)

// Preserve the standard client redirect machinery above the inert transport.
func (self subnetArtifactRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// Native defaults cannot turn a redirect or ambient proxy into origin authority.
func TestSubnetArtifactsNativeClientRejectsRedirects(t *testing.T) {
	client, transport := newSubnetArtifactHttpClient()
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil || !transport.DisableCompression || transport.MaxResponseHeaderBytes != 16*1024 || transport.TLSClientConfig != nil || client.Jar != nil || client.Timeout != subnetArtifactReadBudget {
		t.Fatal("native transport lost its bounded direct TLS profile")
	}
	calls := 0
	client.Transport = subnetArtifactRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": []string{"https://unapproved.private.example.test/"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://first.private.example.test/sn/artifact", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil || calls != 1 || response == nil || response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatal("native client followed a redirect")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

// A read-only source lacking optional HTTP support is an explicit capability
// gap; public Signal.Run must not create a native fallback client.
func TestSubnetArtifactsMissingInjectedCapabilityStaysUnknown(t *testing.T) {
	settings, _, _, _ := subnetArtifactFixture(t)
	settings.Source = &syntheticSource{}
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatal("missing HTTP source appeared healthy")
	}
}

// The actual registry contains exactly one usable new constructor and preserves
// all metadata needed for focused execution and visibility classification.
func TestSubnetArtifactsRegistryContract(t *testing.T) {
	selected, err := IncludeSignals(NewSignals(), "subnet-artifacts")
	if err != nil || len(selected) != 1 {
		t.Fatal("subnet artifact reader is absent or duplicated")
	}
	signal := selected[0]
	if signal.Number() != "22.8a" || signal.ID() != "subnet/artifacts" || signal.Cadence() != time.Minute || !isVisibilityAlertClass("subnet-history-availability") {
		t.Fatal("partial reader registration changed")
	}
}

// The public reducer treats a live-parent bounded-read timeout as one origin's
// missing evidence, retaining the successful sibling and completing the census.
func TestSubnetArtifactsChildTimeoutPreservesHealthySibling(t *testing.T) {
	settings, source, artifact, raw := subnetArtifactFixture(t)
	source.do = func(request *http.Request) (*http.Response, error) {
		if source.calls == 1 {
			return nil, fmt.Errorf("private-child-timeout: %w", context.DeadlineExceeded)
		}
		return subnetArtifactTestResponse(request, raw, artifact.ContentHash), nil
	}
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || source.calls != 2 || len(alerts) != 1 || !alerts[0].unobservable {
		t.Fatal("child timeout aborted its healthy sibling or became success")
	}
	requireAlertOmits(t, alerts[0], "private-child-timeout")
}

// Cancellation while completing the independent authority check preserves an
// already returned source cause rather than erasing the earlier boundary.
func TestSubnetArtifactsCompletionCancellationRetainsReadCause(t *testing.T) {
	settings, source, _, _ := subnetArtifactFixture(t)
	leaf := &subnetArtifactPrivateCause{}
	checks := 0
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
		checks++
		if checks == 2 {
			return false, context.Canceled
		}
		return true, nil
	}
	source.do = func(*http.Request) (*http.Response, error) { return nil, leaf }
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	var actual *subnetArtifactPrivateCause
	if len(alerts) != 0 || source.calls != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, leaf) || !errors.As(err, &actual) || actual != leaf {
		t.Fatal("completion cancellation erased the returned source cause")
	}
	if strings.Contains(err.Error(), "private-") {
		t.Fatal("completion cancellation exposed source text")
	}
}

// Even an otherwise valid original object must match its separately approved
// complete-wire pin; a content hash or valid signature alone cannot replace it.
func TestSubnetArtifactsRequiresIndependentWirePin(t *testing.T) {
	settings, source, _, _ := subnetArtifactFixture(t)
	settings.SubnetArtifacts.Objects[0].WireSha256 = strings.Repeat("ab", 32)
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || source.calls != 2 || len(alerts) != 2 || !strings.Contains(alerts[0].Observed, "artifact-wire-mismatch") {
		t.Fatal("self-consistent object replaced the independent wire pin")
	}
}

// A successful prefix cannot truncate the configured object census. A later
// missing object/origin still has its own stable visibility finding.
func TestSubnetArtifactsReadsEveryEnrolledObject(t *testing.T) {
	settings, source, first, firstRaw := subnetArtifactFixture(t)
	second := *first
	second.NoID = 2
	signSubnetArtifactTest(t, &second)
	secondRaw, err := payoutartifact.Bytes(&second)
	if err != nil {
		t.Fatal(err)
	}
	wireHash := sha256.Sum256(secondRaw)
	expected := settings.SubnetArtifacts.Objects[0]
	expected.NoId, expected.ContentHash, expected.WireSha256 = second.NoID, second.ContentHash, hex.EncodeToString(wireHash[:])
	settings.SubnetArtifacts.Objects = append(settings.SubnetArtifacts.Objects, expected)
	source.do = func(request *http.Request) (*http.Response, error) {
		if request.URL.Query().Get("hash") == first.ContentHash {
			return subnetArtifactTestResponse(request, firstRaw, first.ContentHash), nil
		}
		if request.URL.Query().Get("hash") != second.ContentHash {
			t.Fatal("request selected an unenrolled object")
		}
		response := subnetArtifactTestResponse(request, secondRaw, second.ContentHash)
		if source.calls == 4 {
			response.StatusCode = http.StatusNotFound
		}
		return response, nil
	}
	alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
	if err != nil || source.calls != 4 || len(alerts) != 1 || alerts[0].Target != "subnet-artifacts/object-2/origin-2" || !alerts[0].unobservable {
		t.Fatal("healthy prefix concealed the later missing object")
	}
}

// The last generation callback arms only its completion clock. That clock
// cancels a live parent after the last source result, while returning valid time.
func TestSubnetArtifactsFinalAuthorityClockCancellationWithholdsVerdicts(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, sourceFailure := range []bool{false, true} {
			for _, deadline := range []bool{false, true} {
				settings, source, artifact, raw := subnetArtifactFixture(t)
				if scoped {
					settings.ExcludedHosts = []string{"pg-1"}
				}
				var parent context.Context
				var stop func()
				want := error(context.Canceled)
				if deadline {
					controlled := &subnetArtifactEntryDeadline{Context: context.Background(), done: make(chan struct{})}
					parent, stop, want = controlled, controlled.expire, context.DeadlineExceeded
				} else {
					parent, stop = context.WithCancel(context.Background())
				}
				now := settings.Now()
				checks := 0
				var runOwner context.Context
				armed, canceledAtClock := false, false
				settings.SettingsGenerationCheck = func(ctx context.Context, _ SignalSettings) (bool, error) {
					checks++
					if source.calls == 2 {
						if checks != 3 || parent.Err() != nil || canceledAtClock {
							t.Fatal("final authority was not reached with a live owner")
						}
						if ctx.Err() != nil || ctx.Done() == nil || ctx.Done() == parent.Done() {
							t.Fatal("final authority did not expose the live adapter run owner")
						}
						select {
						case <-ctx.Done():
							t.Fatal("adapter run owner ended before the final clock")
						default:
						}
						runOwner = ctx
						armed = true
					}
					return true, nil
				}
				settings.Now = func() time.Time {
					if armed && !canceledAtClock {
						canceledAtClock = true
						stop()
						// The custom parent propagates asynchronously to this live
						// run owner; completed per-read children cannot prove it.
						<-runOwner.Done()
						if !errors.Is(runOwner.Err(), want) {
							t.Fatal("final clock did not terminate the actual run owner")
						}
					}
					return now
				}
				leaf := &subnetArtifactPrivateCause{}
				source.do = func(request *http.Request) (*http.Response, error) {
					if parent.Err() != nil || canceledAtClock {
						t.Fatal("contact continued after completion clock cancellation")
					}
					if source.calls == 2 && sourceFailure {
						return nil, leaf
					}
					return subnetArtifactTestResponse(request, raw, artifact.ContentHash), nil
				}
				alerts, err := NewSubnetArtifactsSignal().Run(parent, settings)
				stop()
				if source.calls != 2 || checks != 3 || !canceledAtClock {
					t.Fatal("final-clock control did not reach both sources and its final authority transition")
				}
				if err != nil && !errors.Is(err, want) {
					t.Fatal("final-clock control returned an unrelated error")
				}
				if source.calls != 2 || checks != 3 || !canceledAtClock || !errors.Is(err, want) || len(alerts) != 0 {
					t.Fatalf("scoped=%v sourceFailure=%v deadline=%v: final canceled owner published a verdict or missed the final clock", scoped, sourceFailure, deadline)
				}
				if sourceFailure {
					var actual *subnetArtifactPrivateCause
					if !errors.Is(err, leaf) || !errors.As(err, &actual) || actual != leaf {
						t.Fatal("final clock cancellation erased the returned source cause")
					}
				}
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("final clock cancellation exposed source text")
				}
			}
		}
	}
}

// A body can observe expiration during either reading or closing. These cases
// wait on the actual request owner; no source-declared deadline drives the test.
type subnetArtifactDeadlineBody struct {
	reader   io.Reader
	ctx      context.Context
	stage    string
	leaf     error
	ownerErr error
	closes   int
}

// The read failure joins a cleanup cancellation only after the owned deadline.
func (self *subnetArtifactDeadlineBody) Read(buffer []byte) (int, error) {
	if self.stage == "read" {
		<-self.ctx.Done()
		self.ownerErr = self.ctx.Err()
		return 0, errors.Join(self.ownerErr, context.Canceled, self.leaf)
	}
	return self.reader.Read(buffer)
}

// Closing remains exactly once even when its cleanup reports cancellation.
func (self *subnetArtifactDeadlineBody) Close() error {
	self.closes++
	if self.stage == "close" {
		<-self.ctx.Done()
		self.ownerErr = self.ctx.Err()
		return errors.Join(self.ownerErr, context.Canceled, self.leaf)
	}
	return nil
}

// The public reader uses its unchanged five-second budget at every boundary.
// Actual child expiration leaves one origin unknown and still reads its sibling.
func TestSubnetArtifactsOwnedDeadlineCleanupCancellationPreservesSibling(t *testing.T) {
	for _, stage := range []string{"do", "read", "close"} {
		for _, scoped := range []bool{false, true} {
			settings, source, artifact, raw := subnetArtifactFixture(t)
			if scoped {
				settings.ExcludedHosts = []string{"pg-1"}
			}
			parent, cancel := context.WithCancel(context.Background())
			leaf := &subnetArtifactPrivateCause{}
			var body *subnetArtifactDeadlineBody
			var firstOwnerErr error
			source.do = func(request *http.Request) (*http.Response, error) {
				if parent.Err() != nil {
					t.Fatal("child expiration canceled the shared parent")
				}
				if source.calls == 1 {
					if stage == "do" {
						<-request.Context().Done()
						firstOwnerErr = request.Context().Err()
						return nil, errors.Join(firstOwnerErr, context.Canceled, leaf)
					}
					body = &subnetArtifactDeadlineBody{reader: bytes.NewReader(raw), ctx: request.Context(), stage: stage, leaf: leaf}
					response := subnetArtifactTestResponse(request, raw, artifact.ContentHash)
					response.Body = body
					return response, nil
				}
				return subnetArtifactTestResponse(request, raw, artifact.ContentHash), nil
			}
			alerts, err := NewSubnetArtifactsSignal().Run(parent, settings)
			parentErr := parent.Err()
			cancel()
			if body != nil {
				firstOwnerErr = body.ownerErr
			}
			if source.calls == 0 || parentErr != nil || firstOwnerErr != context.DeadlineExceeded {
				t.Fatal("cleanup control did not reach actual child expiry with a live parent")
			}
			var retained *subnetArtifactPrivateCause
			if err != nil && (!errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, leaf) || !errors.As(err, &retained) || retained != leaf) {
				t.Fatal("cleanup control returned an unrelated error or lost the source cause")
			}
			if err != nil || parentErr != nil || firstOwnerErr != context.DeadlineExceeded || source.calls != 2 || len(alerts) != 1 {
				t.Fatalf("%s scoped=%v: expired child aborted its healthy sibling", stage, scoped)
			}
			if body != nil && body.closes != 1 {
				t.Fatal("expired child body lost its close owner")
			}
			alert := requireAlertClass(t, alerts, "subnet-history-availability")
			if !alert.unobservable || alert.Target != "subnet-artifacts/object-1/origin-1" || alert.Sustain != 2 {
				t.Fatal("expired origin was lost or changed availability policy")
			}
			requireAlertOmits(t, alert, leaf.Error())
		}
	}
}

// An unexpired child cannot borrow timeout policy from a source-supplied
// DeadlineExceeded sentinel joined with its cancellation and private cause.
func TestSubnetArtifactsSourceClaimedDeadlineCannotOverrideCancellation(t *testing.T) {
	for _, stage := range []string{"do", "read", "close"} {
		for _, scoped := range []bool{false, true} {
			settings, source, artifact, raw := subnetArtifactFixture(t)
			if scoped {
				settings.ExcludedHosts = []string{"pg-1"}
			}
			leaf := &subnetArtifactPrivateCause{}
			cause := fmt.Errorf("private-wrapper: %w", errors.Join(context.Canceled, context.DeadlineExceeded, leaf))
			source.do = func(request *http.Request) (*http.Response, error) {
				if request.Context().Err() != nil {
					t.Fatal("source-claimed deadline control lost its live read owner")
				}
				if stage == "do" {
					return nil, cause
				}
				body := &subnetArtifactTestBody{reader: bytes.NewReader(raw)}
				if stage == "read" {
					body.readErr = cause
				} else {
					body.closeErr = cause
				}
				response := subnetArtifactTestResponse(request, raw, artifact.ContentHash)
				response.Body = body
				return response, nil
			}
			alerts, err := NewSubnetArtifactsSignal().Run(context.Background(), settings)
			var actual *subnetArtifactPrivateCause
			if source.calls != 1 {
				t.Fatal("source-claimed deadline control did not enter exactly one source")
			}
			if !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, leaf) || !errors.As(err, &actual) || actual != leaf || source.calls != 1 || len(alerts) != 0 {
				t.Fatalf("%s scoped=%v: source claimed child expiry or lost its cause", stage, scoped)
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("source-claimed deadline exposed private text")
			}
		}
	}
}

// The owned child state is sampled before cancel, separately from the retained
// error chain. A later parent cancellation still wins and preserves that chain.
func TestSubnetArtifactsOwnedDeadlineRetainsCauseAndParentPrecedence(t *testing.T) {
	for _, stage := range []string{"do", "read", "close"} {
		settings, source, artifact, raw := subnetArtifactFixture(t)
		parent, cancel := context.WithCancel(context.Background())
		leaf := &subnetArtifactPrivateCause{}
		var body *subnetArtifactDeadlineBody
		source.do = func(request *http.Request) (*http.Response, error) {
			if stage == "do" {
				<-request.Context().Done()
				return nil, errors.Join(request.Context().Err(), context.Canceled, leaf)
			}
			body = &subnetArtifactDeadlineBody{reader: bytes.NewReader(raw), ctx: request.Context(), stage: stage, leaf: leaf}
			response := subnetArtifactTestResponse(request, raw, artifact.ContentHash)
			response.Body = body
			return response, nil
		}
		reason, readErr, readOwnerErr := readSubnetArtifact(parent, source, settings.SubnetArtifacts.Origins[0], settings.SubnetArtifacts.Objects[0], time.Millisecond)
		parentErr := parent.Err()
		liveCancellation := subnetArtifactCancellation(parent, readErr, readOwnerErr)
		cancel()
		if reason == "" || readOwnerErr != context.DeadlineExceeded || parentErr != nil || liveCancellation != nil || source.calls != 1 {
			t.Fatalf("%s: actual owned expiry was not distinct from source cancellation", stage)
		}
		if body != nil && body.closes != 1 {
			t.Fatal("expired read lost body closure")
		}
		var actual *subnetArtifactPrivateCause
		if !errors.Is(readErr, context.Canceled) || !errors.Is(readErr, context.DeadlineExceeded) || !errors.Is(readErr, leaf) || !errors.As(readErr, &actual) || actual != leaf {
			t.Fatal("owned timeout erased its cleanup or typed source cause")
		}
		err := subnetArtifactCancellation(parent, readErr, readOwnerErr)
		actual = nil
		if !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, leaf) || !errors.As(err, &actual) || actual != leaf {
			t.Fatal("expired child masked parent cancellation or lost the original source cause")
		}
		if strings.Contains(err.Error(), "private-") {
			t.Fatal("parent cancellation exposed timeout source text")
		}
	}
}
