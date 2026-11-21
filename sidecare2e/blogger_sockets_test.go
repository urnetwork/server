//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The blogger case's view of sockets' registration calls (blogger_test.go): the world's sockets origin served through
// a recorder in front of the world's lb for the block (worldLb), which keeps every POST /socket/registrations as the
// sockets block took and answered it: when, which client called (its service token's ur_client_id, read from the
// token's claims unverified, since the block verifies it), the page's set_version and entries, and the answer's
// status and body. It changes nothing the call carries. A listen that never takes is reported with its client's calls.
// Safe for concurrent use.

package sidecare2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// What the recorder keeps of each call (QD-702).
type bloggerSocketCallSettings struct {
	// The most bytes of a request's and an answer's body kept, and the most calls kept, the oldest going first.
	BodyMaxByteCount int
	CallMaxCount     int
	// The most bytes of the process's goroutine stacks a listen that never takes reads.
	StackMaxByteCount int
}

func defaultBloggerSocketCallSettings() *bloggerSocketCallSettings {
	return &bloggerSocketCallSettings{
		BodyMaxByteCount:  2048,
		CallMaxCount:      4096,
		StackMaxByteCount: 64 * 1024 * 1024,
	}
}

// One registration call as sockets took and answered it: kept at its arrival, so one that never comes back shows.
type bloggerSocketCall struct {
	at       time.Time
	clientId string
	// the page's set_version and each entry's name and listen id
	setVersion uint64
	entries    []string
	// set once sockets answered
	answered   bool
	answeredAt time.Time
	status     int
	answer     []byte
}

// The recorder: see the file header.
type bloggerSocketCalls struct {
	next     http.Handler
	settings *bloggerSocketCallSettings

	stateLock sync.Mutex
	calls     []*bloggerSocketCall
}

// Serves the world's sockets origin through a recorder from now on, in place of the world's own handler for it.
func installBloggerSocketCalls(world *World, settings *bloggerSocketCallSettings) (*bloggerSocketCalls, error) {
	block := world.Block(WorldBlockSockets)
	if block == nil {
		return nil, fmt.Errorf("the world runs no %s", WorldBlockSockets)
	}
	recorder := &bloggerSocketCalls{next: &worldLb{block: block}, settings: settings}
	api := world.T3().Api()
	if err := api.ServeService(block.service, nil); err != nil {
		return nil, err
	}
	if err := api.ServeService(block.service, recorder); err != nil {
		return nil, err
	}
	return recorder, nil
}

func (self *bloggerSocketCalls) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/socket/registrations" {
		self.next.ServeHTTP(writer, request)
		return
	}
	body, err := io.ReadAll(request.Body)
	request.Body.Close()
	if err != nil {
		http.Error(writer, "the request's body", http.StatusBadRequest)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	call := &bloggerSocketCall{at: time.Now(), clientId: bloggerTokenClientId(request.Header.Get("Authorization"))}
	page := &struct {
		SetVersion    uint64 `json:"set_version"`
		Registrations []struct {
			Name     string `json:"name"`
			ListenId string `json:"listen_id"`
			Serving  *bool  `json:"serving"`
		} `json:"registrations"`
	}{}
	if json.Unmarshal(body, page) == nil {
		call.setVersion = page.SetVersion
		for _, registration := range page.Registrations {
			serving := "serving"
			if registration.Serving != nil && !*registration.Serving {
				serving = "not serving"
			}
			call.entries = append(call.entries, fmt.Sprintf("%s %s %s", registration.Name, registration.ListenId, serving))
		}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.calls = append(self.calls, call)
		if excess := len(self.calls) - self.settings.CallMaxCount; 0 < excess {
			self.calls = slices.Clone(self.calls[excess:])
		}
	}()
	recording := &bloggerRecordingResponse{ResponseWriter: writer, status: http.StatusOK, limit: self.settings.BodyMaxByteCount}
	self.next.ServeHTTP(recording, request)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	call.answered = true
	call.answeredAt = time.Now()
	call.status = recording.status
	call.answer = recording.body.Bytes()
}

// The calls clientId made at or after since, one line each.
func (self *bloggerSocketCalls) describe(clientId string, since time.Time) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lines := []string{}
	for _, call := range self.calls {
		if call.clientId != clientId || call.at.Before(since) {
			continue
		}
		if !call.answered {
			lines = append(lines, fmt.Sprintf("%s set %d [%s] never answered", call.at.Format("15:04:05.000"), call.setVersion, strings.Join(call.entries, ", ")))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s set %d [%s] answered %d at %s %s", call.at.Format("15:04:05.000"), call.setVersion, strings.Join(call.entries, ", "), call.status, call.answeredAt.Format("15:04:05.000"), strings.TrimSpace(string(call.answer))))
	}
	if len(lines) == 0 {
		return "no registration call since"
	}
	return strings.Join(lines, " | ")
}

// Each client's count of calls and its last call's time, at or after since: whether the registrars still call.
func (self *bloggerSocketCalls) summary(since time.Time) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clientIdCounts := map[string]int{}
	clientIdLasts := map[string]time.Time{}
	for _, call := range self.calls {
		if call.at.Before(since) {
			continue
		}
		clientIdCounts[call.clientId] += 1
		clientIdLasts[call.clientId] = call.at
	}
	clientIds := []string{}
	for clientId := range clientIdCounts {
		clientIds = append(clientIds, clientId)
	}
	slices.Sort(clientIds)
	lines := []string{}
	for _, clientId := range clientIds {
		lines = append(lines, fmt.Sprintf("%s %d calls, the last at %s", clientId, clientIdCounts[clientId], clientIdLasts[clientId].Format("15:04:05.000")))
	}
	return strings.Join(lines, "; ")
}

// The stacks of every goroutine whose stack names any of names, for a call that never comes: where the registrar's
// loop and sockets' handler stand. At most maxByteCount bytes of the process's stacks are read.
func bloggerGoroutineStacks(maxByteCount int, names ...string) string {
	buffer := make([]byte, maxByteCount)
	stacks := string(buffer[:runtime.Stack(buffer, true)])
	kept := []string{}
	for _, stack := range strings.Split(stacks, "\n\n") {
		for _, name := range names {
			if strings.Contains(stack, name) {
				kept = append(kept, stack)
				break
			}
		}
	}
	return strings.Join(kept, "\n\n")
}

// The ur_client_id a bearer service token's claims name, "" where it names none: read unverified, for the record
// alone.
func bloggerTokenClientId(authorization string) string {
	token, found := strings.CutPrefix(authorization, "Bearer ")
	if !found {
		return ""
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	claims := &struct {
		ClientId string `json:"ur_client_id"`
	}{}
	if json.Unmarshal(payload, claims) != nil {
		return ""
	}
	return claims.ClientId
}

// A response writer that keeps the status and the first bytes of the body it passes on.
type bloggerRecordingResponse struct {
	http.ResponseWriter
	status int
	limit  int
	body   bytes.Buffer
}

func (self *bloggerRecordingResponse) WriteHeader(status int) {
	self.status = status
	self.ResponseWriter.WriteHeader(status)
}

func (self *bloggerRecordingResponse) Write(data []byte) (int, error) {
	if room := self.limit - self.body.Len(); 0 < room {
		self.body.Write(data[:min(room, len(data))])
	}
	return self.ResponseWriter.Write(data)
}

// Passes a flush on, for a handler that streams.
func (self *bloggerRecordingResponse) Flush() {
	if flusher, ok := self.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
