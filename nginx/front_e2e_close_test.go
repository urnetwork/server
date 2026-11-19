package nginx

// End-to-end tests of Front.Close (D12, D21): it releases what outlived the
// drain, and when it reports the forced close complete, every spill file is
// closed and its budget returned. The body store's close, which keeps that
// promise for the spill files, is also held at its own layer.

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A spill file whose Close says it began and then waits for the test, as a
// close on a slow disk would, so a test can hold a release halfway.
type e2eHeldCloseFile struct {
	bodySpillFile
	// closed when the first Close begins
	closing chan struct{}
	// closed by the test to let Close finish
	proceed     chan struct{}
	closingOnce sync.Once
}

// See io.Closer.
func (self *e2eHeldCloseFile) Close() error {
	self.closingOnce.Do(func() {
		close(self.closing)
	})
	<-self.proceed
	return self.bodySpillFile.Close()
}

// Close never reports the forced close complete while a spill file is still
// being released. A body whose request has ended is released by its
// request's goroutine; a Close that comes in while that release is closing
// the file must wait for it, and return the forced close's error at
// HttpSettings.ForcedCloseTimeout if it can't, never nil with the file open
// and its budget held (D21: every exit releases bytes, descriptors and
// files).
//
// The front's spill files are created through the store's own seam for
// them, so the first file's Close can be held: that holds the request's
// release halfway, with the body marked released and its file and budget
// not yet returned.
func TestFrontE2eCloseNeverReportsASpillFileStillOpen(t *testing.T) {
	settings := e2eFrontSettings(t)
	// with a release held, Close can only end at this bound
	settings.Http.ForcedCloseTimeout = 1 * time.Second
	e2e := newE2eFront(t, settings, nil)
	eth1 := e2e.iface(t, "eth1")
	store := e2e.store(t)
	closing := make(chan struct{})
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	letCloseProceed := func() {
		proceedOnce.Do(func() {
			close(proceed)
		})
	}
	t.Cleanup(letCloseProceed)
	// set before any request reaches the store
	store.createSpillFile = func() (bodySpillFile, error) {
		file, err := store.createTempSpillFile()
		if err != nil {
			return nil, err
		}
		return &e2eHeldCloseFile{
			bodySpillFile: file,
			closing:       closing,
			proceed:       proceed,
		}, nil
	}

	client := e2e.newH1Client(t, eth1.httpsAddr, "web.service.example")
	response, body := e2eDo(t, client, newE2eRequest(t, "POST", "https://web.service.example/spilled", bytes.NewReader(e2eBody(100*1024))))
	if response.StatusCode != http.StatusOK || string(body) != "web/g1" {
		t.Fatalf("the spilled request: %d %q", response.StatusCode, body)
	}
	// the request's own release is closing the spill file
	waitE2eClosed(t, closing, "the request's release of its spill file")

	if err := e2e.front.Close(); err == nil {
		if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
			t.Errorf("Close reported the forced close complete while a spill file was still closing: the store holds %d bytes, %d spilled in %d files, for %d bodies", memoryBytes, spillBytes, spillFiles, bodies)
		}
	}
	letCloseProceed()
	waitE2eClosed(t, e2e.front.cutDone, "the forced close")
	// the request's release runs before its handler returns
	eth1.httpsServed.await(t, "/spilled")
	if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
		t.Errorf("once the release finished, the store holds %d bytes, %d spilled in %d files, for %d bodies", memoryBytes, spillBytes, spillFiles, bodies)
	}
}

// Close never reports the forced close complete while a spill file is still
// being created. A Close that comes in between the file's creation and its
// body's record of it releases the body without seeing the file. It must
// wait for the body's append, which finds the body released and closes the
// file, and return the forced close's error at
// HttpSettings.ForcedCloseTimeout if it can't, never nil with the file open
// (D21). The request it cut short is a local 503 that reaches no block.
//
// The store's seam for spill files holds the creation once its file is
// open, so Close comes in at that point.
func TestFrontE2eCloseNeverReportsASpillFileBeingCreated(t *testing.T) {
	settings := e2eFrontSettings(t)
	// with a creation held, Close can only end at this bound
	settings.Http.ForcedCloseTimeout = 1 * time.Second
	e2e := newE2eFront(t, settings, nil)
	eth1 := e2e.iface(t, "eth1")
	store := e2e.store(t)
	hold := newE2eHold(t)
	createdFiles := make(chan *os.File, 1)
	// set before any request reaches the store
	store.createSpillFile = func() (bodySpillFile, error) {
		file, err := store.createTempSpillFile()
		if err != nil {
			return nil, err
		}
		createdFiles <- file.(*os.File)
		// the file is open, and its body hasn't recorded it
		<-hold.released
		return file, nil
	}

	client := e2e.newH1Client(t, eth1.httpsAddr, "web.service.example")
	request := newE2eRequest(t, "POST", "https://web.service.example/spilling", bytes.NewReader(e2eBody(100*1024)))
	requestCounts := e2e.requestCounts()
	statuses := make(chan int, 1)
	go func() {
		response, err := client.Do(request)
		if err != nil {
			statuses <- 0
			return
		}
		response.Body.Close()
		statuses <- response.StatusCode
	}()
	spillFile := receiveE2e(t, createdFiles, "spill file")
	spillFileClosed := func() bool {
		_, err := spillFile.Stat()
		return errors.Is(err, os.ErrClosed)
	}

	if err := e2e.front.Close(); err == nil && !spillFileClosed() {
		t.Errorf("Close reported the forced close complete while a spill file being created was open")
	}
	hold.release()
	waitE2eClosed(t, e2e.front.cutDone, "the forced close")
	if !spillFileClosed() {
		t.Errorf("the spill file is open after the forced close completed")
	}
	if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
		t.Errorf("after the forced close the store holds %d bytes, %d spilled in %d files, for %d bodies", memoryBytes, spillBytes, spillFiles, bodies)
	}
	if status := receiveE2e(t, statuses, "response"); status != http.StatusServiceUnavailable {
		t.Errorf("the request whose body was spilling = %d, want the local 503", status)
	}
	if requests := e2e.requestsSince(requestCounts); len(requests) != 0 {
		t.Errorf("the request whose body was spilling reached %v", e2eRequestServices(requests))
	}
}

// The body store's close, which the forced close runs, returns only once
// every spill file its bodies created is closed, one still being created
// included. A close that releases a body while its append is creating the
// file can't see the file: it waits for the append, which finds the body
// released and closes the file, and the file stays counted against
// SpillMaxFiles until then (D21). In a synctest bubble, so the close is seen
// waiting rather than given time to return.
func TestFrontE2eBodyStoreCloseWaitsForASpillFileBeingCreated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultBodySettings()
		settings.SpillDir = t.TempDir()
		store := newBodyStore(settings)
		created := make(chan struct{})
		proceed := make(chan struct{})
		var spillFile *os.File
		store.createSpillFile = func() (bodySpillFile, error) {
			file, err := store.createTempSpillFile()
			if err != nil {
				return nil, err
			}
			spillFile = file.(*os.File)
			close(created)
			// the file is open, and its body hasn't recorded it
			<-proceed
			return file, nil
		}
		refusals := make(chan *bodyRefusal, 1)
		go func() {
			body := e2eBody(int(settings.MemoryPrefixBytes) + 1000)
			storedBody, refusal := store.read(t.Context(), bytes.NewReader(body), bodyReadOptions{
				declaredLength: int64(len(body)),
				prefixBytes:    settings.MemoryPrefixBytes,
			})
			if storedBody != nil {
				storedBody.release()
			}
			refusals <- refusal
		}()
		<-created
		spillFileClosed := func() bool {
			_, err := spillFile.Stat()
			return errors.Is(err, os.ErrClosed)
		}

		closeDone := make(chan struct{})
		go func() {
			store.close()
			close(closeDone)
		}()
		// every other goroutine is durably blocked: the append at proceed,
		// and the close returned or waiting for the append
		synctest.Wait()
		select {
		case <-closeDone:
			if !spillFileClosed() {
				t.Errorf("the store's close returned while a spill file being created was open")
			}
		default:
		}
		if _, _, spillFiles, _ := store.usage(); spillFiles != 1 {
			t.Errorf("with a spill file open, the store counts %d spill files, want 1", spillFiles)
		}

		close(proceed)
		<-closeDone
		if !spillFileClosed() {
			t.Errorf("the store's close returned with the spill file open")
		}
		if refusal := <-refusals; refusal == nil || refusal.status != http.StatusServiceUnavailable || refusal.reason != bodyRefusalClosed {
			t.Errorf("the body read the close cut short: %v, want refused as closed", refusal)
		}
		if memoryBytes, spillBytes, spillFiles, bodies := store.usage(); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
			t.Errorf("after the close the store holds %d bytes, %d spilled in %d files, for %d bodies", memoryBytes, spillBytes, spillFiles, bodies)
		}
	})
}

// Close releases what the front holds: its pooled upstream connections, the
// upstream connection of a request in flight, an open tunnel's connections,
// and every stored body's memory and spill file. The request in flight is
// answered 502, since no block is left to retry on, and so is nothing more:
// requests after Close are 503s that reach no block.
func TestFrontE2eCloseReleasesUpstreamConnectionsAndSpillFiles(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	client := e2e.newH1Client(t, eth1.httpsAddr, "web.service.example")

	// a pooled connection to web/g1: both requests take the same one
	web := e2e.upstream(t, "web", "g1")
	for _, path := range []string{"/pooled-first", "/pooled-second"} {
		response, _ := e2eDo(t, client, newE2eRequest(t, "GET", "https://web.service.example"+path, nil))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, response.StatusCode)
		}
	}
	webRequests := web.awaitRequests(t, 2)
	if webRequests[0].remoteAddr != webRequests[1].remoteAddr {
		t.Fatalf("the front dialed web/g1 twice (%s, %s), want its pooled connection reused", webRequests[0].remoteAddr, webRequests[1].remoteAddr)
	}
	web.awaitOpenConns(t, 1)

	// an open tunnel
	websocketBlock := newE2eWebsocketBlock()
	e2e.upstream(t, "gossip", "g1").setHandler(websocketBlock.serveHttp)
	conn, _, err := e2e.dialE2eWebsocket(t, eth1.httpsAddr, "/ws")
	if err != nil {
		t.Fatalf("the websocket dial failed: %v", err)
	}
	if greeting := readE2eWebsocketMessage(t, conn); greeting != "welcome" {
		t.Fatalf("the client read %q first, want the block's greeting", greeting)
	}

	// a request held at its block with its body spilled
	hold := newE2eHold(t)
	e2e.setServiceHandler(t, "api", func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		hold.wait(r)
		w.WriteHeader(http.StatusOK)
	})
	apiClient := e2e.newH1Client(t, eth1.httpsAddr, "api.service.example")
	body := e2eBody(100 * 1024)
	heldStatuses := make(chan int, 1)
	go func() {
		response, err := apiClient.Do(newE2eRequest(t, "POST", "https://api.service.example/held", bytes.NewReader(body)))
		if err != nil {
			heldStatuses <- 0
			return
		}
		response.Body.Close()
		heldStatuses <- response.StatusCode
	}()
	// the request reaches one of the two blocks
	heldUpstream := e2e.upstream(t, "api", e2e.awaitServiceRequests(t, "api", 1)[0].block)
	if _, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); spillFiles != 1 || spillBytes != int64(len(body))-e2e.settings.Body.MemoryPrefixBytes || bodies != 1 {
		t.Fatalf("the held body: %d bytes spilled in %d files, %d bodies, want its spill held", spillBytes, spillFiles, bodies)
	}

	if err := e2e.front.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
		t.Errorf("after Close the store holds %d bytes, %d spilled in %d files, for %d bodies", memoryBytes, spillBytes, spillFiles, bodies)
	}
	if names := e2e.spillDirNames(t); len(names) != 0 {
		t.Errorf("after Close the spill directory lists %v", names)
	}
	web.awaitOpenConns(t, 0)
	heldUpstream.awaitOpenConns(t, 0)
	waitE2eClosed(t, websocketBlock.ended, "the block's end of the tunnel")
	conn.SetReadDeadline(time.Now().Add(e2eWaitTimeout))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Errorf("the client's tunnel still reads after Close")
	} else if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
		t.Errorf("the client's tunnel was left open after Close")
	}
	if status := receiveE2e(t, heldStatuses, "held response"); status != http.StatusBadGateway {
		t.Errorf("the request in flight at Close = %d, want 502", status)
	}

	requestCounts := e2e.requestCounts()
	response, _ := e2eDo(t, client, newE2eRequest(t, "GET", "https://web.service.example/after-close", nil))
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a request after Close = %d, want 503", response.StatusCode)
	}
	if requests := e2e.requestsSince(requestCounts); len(requests) != 0 {
		t.Errorf("a request after Close reached %v", e2eRequestServices(requests))
	}
}

// Close never reports the forced close complete while an upstream dial is
// in flight with its connection open. The close ends a dial through its
// context; one that has its connection and doesn't return it is waited for,
// up to HttpSettings.ForcedCloseTimeout, and closes the connection itself
// once it finds its pool closed. So Close returns the forced close's error
// rather than nil while the dial holds, and the request it served is a 502,
// with no block left to try. The api group's dial seam has its connection
// and holds it until the test lets it go, whatever its context says.
func TestFrontE2eCloseNeverReportsADialStillOpen(t *testing.T) {
	settings := e2eFrontSettings(t)
	// with a dial held, Close can only end at this bound
	settings.Http.ForcedCloseTimeout = 1 * time.Second
	e2e := newE2eFront(t, settings, nil)
	eth1 := e2e.iface(t, "eth1")
	group := e2e.front.serviceForwarders["api"].(*upstreamGroup)
	hold := newE2eHold(t)
	dialedConns := make(chan net.Conn, 1)
	dialer := &net.Dialer{}
	// set before any request reaches the group
	group.dialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		dialedConns <- conn
		<-hold.released
		return conn, nil
	}

	client := e2e.newH1Client(t, eth1.httpsAddr, "api.service.example")
	statuses := make(chan int, 1)
	go func() {
		response, err := client.Do(newE2eRequest(t, "GET", "https://api.service.example/dialing", nil))
		if err != nil {
			statuses <- 0
			return
		}
		response.Body.Close()
		statuses <- response.StatusCode
	}()
	dialedConn := receiveE2e(t, dialedConns, "dial")
	dialedConnOpen := func() bool {
		return dialedConn.SetDeadline(time.Time{}) == nil
	}

	if err := e2e.front.Close(); err == nil && dialedConnOpen() {
		t.Errorf("Close reported the forced close complete with a dial's connection open")
	}
	hold.release()
	waitE2eClosed(t, e2e.front.cutDone, "the forced close")
	if dialedConnOpen() {
		t.Errorf("the dial's connection is open after the forced close completed")
	}
	if status := receiveE2e(t, statuses, "response"); status != http.StatusBadGateway {
		t.Errorf("the request whose dial was in flight = %d, want 502", status)
	}
}
