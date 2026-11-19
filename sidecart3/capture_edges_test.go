//go:build sidecar_localstack && sidecar_pending

// CC4's M5 sources on T3 (sidecartest's privacy_edges.go; TEST-M5-01): the edge records the scan reads are the rows
// server's own stores wrote to the test's Postgres, model.TlsEdgeStore's and model.MxEdgeStore's, read back as the
// engine's types; a query handed to the resolver outside URnetwork and a Find over its transport are held on a T3 Net
// as on T2's; and each M5 class's leak is caught on a T3 Net under sidecar_leaktest. Like net_test.go these fail
// rather than skip without the stack.

package sidecart3

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// tls_connection_record's rows of one network, as the capture reads them at each scan.
type edgeTlsRecords struct {
	networkId sidecar.Id
	since     time.Time
}

func (self *edgeTlsRecords) Records() []*tlsedgeservice.TlsConnectionRecord {
	return model.Testing_ReadTlsConnectionRecords(context.Background(), self.networkId, self.since)
}

// mail_connection_record's rows of one network, as the capture reads them at each scan.
type edgeMxRecords struct {
	networkId sidecar.Id
	since     time.Time
}

func (self *edgeMxRecords) Records() []*mxedgeservice.MxConnectionRecord {
	return model.Testing_ReadMxConnectionRecords(context.Background(), self.networkId, self.since)
}

// A T3 Net for the edge sources, closed at the test's end.
func newEdgeNet(t testing.TB) *sidecartest.Net {
	t.Helper()
	net := NewNet(t, nil)
	t.Cleanup(net.Close)
	return net
}

// What server's tlsedge store wrote is what the scan reads: a row that keeps to tls-3 reads clean, a server name
// where the published host belongs is found under CC4/edge-record, and so is a visitor hash the test forbade.
func TestT3EdgeRecordsOfServersTlsStoreAreScanned(t *testing.T) {
	captureTestEnv().Run(t, func(tb testing.TB) {
		if !server.SidecarMg5Linked() {
			tb.Fatal("MG5's tables are not linked: build with sidecar_pending")
		}
		net := newEdgeNet(tb)
		capture := net.Capture()
		networkId := sidecar.NewId()
		now := server.NowUtc()
		capture.AddTlsEdgeRecords("tlsedge's store", &edgeTlsRecords{networkId: networkId, since: now.Add(-time.Hour)})
		store := model.NewTlsEdgeStore(nil)
		insert := func(host string, visitorHash []byte) {
			record := &tlsedgeservice.TlsConnectionRecord{
				ConnectionId: sidecar.NewId(), NetworkId: networkId, Host: host,
				StartTime: now.Add(-time.Second), EndTime: now, ByteCount: 4096, VisitorHash: visitorHash,
			}
			if err := store.InsertConnectionRecords(tb.Context(), []*tlsedgeservice.TlsConnectionRecord{record}); err != nil {
				tb.Fatal(err)
			}
		}

		capture.Canary("a mail body")
		insert("www", bytes.Repeat([]byte{7}, 16))
		if capture.ItemCount(sidecartest.PrivacyClassEdgeRecord) == 0 {
			tb.Fatal("the row server's store wrote is not in the capture")
		}
		for _, finding := range capture.Scan() {
			if finding.Class == sidecartest.PrivacyClassEdgeRecord {
				tb.Errorf("a row that keeps to tls-3: %s", finding)
			}
		}

		serverName := capture.CanaryText("a hello's server name")
		insert(serverName, bytes.Repeat([]byte{7}, 16))
		if !caughtIn(capture.Scan(), "a hello's server name", sidecartest.PrivacyClassEdgeRecord) {
			tb.Error("a server name stored as a record's host was not found")
		}
		visitor := []byte("visitor-at-rest!")
		capture.Forbid("a visitor's hash", visitor)
		insert("@", visitor)
		if !caughtIn(capture.Scan(), "a visitor's hash", sidecartest.PrivacyClassEdgeRecord) {
			tb.Error("a forbidden visitor hash stored in a record was not found")
		}
	})
}

// What server's mxedge store wrote is what the scan reads: a session's row reads clean, and a result the test
// forbade, stored through the store, is found under CC4/edge-record.
func TestT3EdgeRecordsOfServersMxStoreAreScanned(t *testing.T) {
	captureTestEnv().Run(t, func(tb testing.TB) {
		if !server.SidecarMg7Linked() {
			tb.Fatal("MG7's tables are not linked: build with sidecar_pending")
		}
		net := newEdgeNet(tb)
		capture := net.Capture()
		networkId := sidecar.NewId()
		now := server.NowUtc()
		capture.AddMxEdgeRecords("mxedge's store", &edgeMxRecords{networkId: networkId, since: now.Add(-time.Hour)})
		store := model.NewMxEdgeStore(nil)
		insert := func(result string) {
			record := &mxedgeservice.MxConnectionRecord{NetworkId: networkId, StartTime: now, ByteCount: 2048, Result: result}
			if err := store.InsertConnectionRecords(tb.Context(), []*mxedgeservice.MxConnectionRecord{record}); err != nil {
				tb.Fatal(err)
			}
		}

		capture.Canary("a mail body")
		insert("delivered")
		if capture.ItemCount(sidecartest.PrivacyClassEdgeRecord) == 0 {
			tb.Fatal("the row server's store wrote is not in the capture")
		}
		for _, finding := range capture.Scan() {
			if finding.Class == sidecartest.PrivacyClassEdgeRecord {
				tb.Errorf("a session's row: %s", finding)
			}
		}
		capture.Forbid("a sender's name", []byte("a_sender_nobody_may_keep"))
		insert("a_sender_nobody_may_keep")
		if !caughtIn(capture.Scan(), "a sender's name", sidecartest.PrivacyClassEdgeRecord) {
			tb.Error("a forbidden value stored as a record's result was not found")
		}
	})
}

// A sockets service that answers every Find with no ticket.
type edgeFindService struct{}

func (self *edgeFindService) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		io.Copy(io.Discard, request.Body)
		request.Body.Close()
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader([]byte(`{}`))), Request: request}, nil
}

// On a T3 Net the resolver outside URnetwork and a Find's transport hold what they were sent: a public name's query
// and a Find by token read clean, and a private name in either is found in its class.
func TestT3EdgeQueriesAndFindsAreScanned(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		net := newEdgeNet(tb)
		capture := net.Capture()
		privateName := capture.CanaryText("a private name")
		resolver := sidecartest.NewFakeUpstreamResolver(net, "the host's resolver")
		ask := func(name string) {
			query := &dns.Msg{}
			query.SetQuestion(dns.Fqdn(name), dns.TypeA)
			packed, err := query.Pack()
			if err != nil {
				tb.Fatal(err)
			}
			if _, err := resolver.Resolve(tb.Context(), packed); err != nil {
				tb.Fatal(err)
			}
		}
		find := func(body string) {
			client := &http.Client{Transport: capture.Finds("tlsedge1").Transport(&edgeFindService{})}
			request, err := http.NewRequestWithContext(tb.Context(), http.MethodPost, "http://sockets.example/socket/find", bytes.NewReader([]byte(body)))
			if err != nil {
				tb.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				tb.Fatal(err)
			}
			response.Body.Close()
		}

		ask("public.example")
		find(`{"network_id":"0f0e0d0c-0b0a-0908-0706-050403020100","name_token":"dG9rZW4="}`)
		if capture.ItemCount(sidecartest.PrivacyClassUpstreamResolver) != 1 || capture.ItemCount(sidecartest.PrivacyClassFind) == 0 {
			tb.Fatalf("the capture holds %d upstream queries and %d Find items", capture.ItemCount(sidecartest.PrivacyClassUpstreamResolver), capture.ItemCount(sidecartest.PrivacyClassFind))
		}
		for _, finding := range capture.Scan() {
			if finding.Class == sidecartest.PrivacyClassUpstreamResolver || finding.Class == sidecartest.PrivacyClassFind {
				tb.Errorf("nothing private was sent: %s", finding)
			}
		}

		ask(privateName + ".internal")
		if !caughtIn(capture.Scan(), "a private name", sidecartest.PrivacyClassUpstreamResolver) {
			tb.Error("a private name handed to the upstream resolver was not found")
		}
		find(`{"network_id":"0f0e0d0c-0b0a-0908-0706-050403020100","name":"` + privateName + `"}`)
		if !caughtIn(capture.Scan(), "a private name", sidecartest.PrivacyClassFind) {
			tb.Error("a private name in a Find's body was not found")
		}
	})
}

// §22.7's self-test on T3 for CC4's M5 classes: each class's leak, put through its real path on a T3 Net, is caught in
// its class. It runs under sidecar_leaktest alone, which links the leaks, as sidecar-t3.sh's leak leg builds.
func TestT3EveryEdgeClassCatchesItsLeak(t *testing.T) {
	if !sidecartest.LeaksLinked {
		t.Skip("the leaks link only under sidecar_leaktest, which the leak leg builds")
	}
	testEnv().Run(t, func(tb testing.TB) {
		leaks := sidecartest.LeakClasses()
		for _, class := range sidecartest.PrivacyEdgeClasses() {
			leak := leaks[class]
			if leak == nil {
				tb.Errorf("%s has no leak", class)
				continue
			}
			func() {
				net := NewNet(tb, nil)
				defer net.Close()
				text := net.Capture().CanaryText("leak")
				leak.Put(tb, net, []byte(text))
				if !caughtIn(net.Capture().Scan(), "leak", class) {
					tb.Errorf("%s did not catch its leak on T3", class)
				}
			}()
		}
	})
}
