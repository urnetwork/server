// The hold on a request to a Pebble authorization while Pebble validates a challenge of it, which keeps Pebble's own
// lock leak from wedging the local tier's Pebble until it restarts. sidecartest/pebble.go, the Pebble a sidecar test
// runs in its own process, holds the same requests the same way (certs-pebble-hold-test-hang). Its hold is
// unexported and bound to testing.TB, and sidecartest links the sidecar module's core, connect and protobuf, so this
// binary keeps a copy of its own; the two read Pebble alike and should change together.
//
// Pebble v2.10.3 leaks a read lock on each challenge it leaves out when it shows an authorization that is no longer
// pending (wfe's prepAuthorizationForDisplay continues past the challenge's RUnlock). A challenge no one starts costs
// nothing by it, but a deactivation that lands while the authorization's challenge is under validation leaves that
// challenge's lock read-held for good: the validation authority then waits forever for the challenge's write lock
// while it holds the authorization's, and a later newOrder, whose look for a valid authorization to reuse reads each
// authorization's lock in turn, waits forever behind it with Pebble's store lock read-held, so no order goes again
// until Pebble restarts. acmez deactivates an order's pending authorizations when a later one's solver fails, or an
// earlier one's validation does, after it has had Pebble start validating them.
//
// So a request on an authorization reaches Pebble only once the validation authority has finished every challenge of
// it under way, which only the authority's log tells: the hold reads it for each challenge set valid or invalid. A
// request still held at ValidationWaitTimeout is answered 503 in Pebble's place and logged, and a stop answers every
// held request 503 so that it waits on none. The hold keeps a challenge's entry only while a request waits on it, so
// what it keeps stays bounded however long Pebble runs.
//
// What the hold does not cover, as the harness's does not: a client starting a challenge while it sends a request on
// the same authorization is not held apart from it, and a challenge started on an authorization Pebble has already
// shown deactivated, valid or invalid waits forever on the lock that showing leaked, as does the hold's own read of
// that challenge on a later request. acmez does neither: it starts a challenge only on an authorization it last read
// pending, and tries a failed challenge type again on a new order.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/letsencrypt/pebble/v2/acme"
	"github.com/letsencrypt/pebble/v2/db"
)

// Where Pebble serves authorizations, wfe's authzPath, which it does not export.
const pebbleAuthzPath = "/authZ/"

// What Pebble's validation authority logs once it has set a challenge and its authorization valid or invalid, after
// "authz <id> set VALID" or "set INVALID" and before the challenge's id.
const pebbleValidationDoneMarker = " by completed challenge "

// Pebble's ACME handler behind the hold (see the file header). Safe for concurrent use.
type pebbleHold struct {
	settings    *PebbleSettings
	memoryStore *db.MemoryStore
	validations *pebbleValidations
	// Pebble's own
	handler http.Handler
	// the operator's, which hears of each request answered here past its bound
	logger *log.Logger
	// closed at the stop, which lets every held request go
	stopping <-chan struct{}
	// closed at the first request held, which a test waits on
	authorizationHeld     chan struct{}
	authorizationHeldOnce sync.Once
}

func newPebbleHold(
	settings *PebbleSettings,
	memoryStore *db.MemoryStore,
	validations *pebbleValidations,
	handler http.Handler,
	logger *log.Logger,
	stopping <-chan struct{},
) *pebbleHold {
	return &pebbleHold{
		settings:          settings,
		memoryStore:       memoryStore,
		validations:       validations,
		handler:           handler,
		logger:            logger,
		stopping:          stopping,
		authorizationHeld: make(chan struct{}),
	}
}

func (self *pebbleHold) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// Pebble's authorization handler takes POST alone
	if request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, pebbleAuthzPath) && !self.awaitValidations(writer, request) {
		return
	}
	self.handler.ServeHTTP(writer, request)
}

// Holds a request on an authorization until the validation authority has finished each challenge of it under way.
// False when the request was answered here, or its client has gone.
func (self *pebbleHold) awaitValidations(writer http.ResponseWriter, request *http.Request) bool {
	authz := self.memoryStore.GetAuthorizationByID(strings.TrimPrefix(request.URL.Path, pebbleAuthzPath))
	if authz == nil {
		// Pebble answers it 404
		return true
	}
	// an authorization's challenges are set before it is stored, and its lock orders before theirs
	authz.RLock()
	challenges := slices.Clone(authz.Challenges)
	authz.RUnlock()
	answerProblem := func(detail string) {
		problem, _ := json.Marshal(map[string]any{
			"type":   "urn:ietf:params:acme:error:serverInternal",
			"detail": detail,
			"status": http.StatusServiceUnavailable,
		})
		writer.Header().Set("Content-Type", "application/problem+json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		writer.Write(problem)
	}
	var timeout <-chan time.Time
	for _, challenge := range challenges {
		challenge.RLock()
		challengeId := challenge.ID
		challenge.RUnlock()
		// watched before its status is read, so a validation that ends in between still lets the request go
		done, unwatch := self.validations.watch(challengeId)
		challenge.RLock()
		validating := challenge.Status == acme.StatusProcessing
		challenge.RUnlock()
		if !validating {
			unwatch()
			continue
		}
		if timeout == nil {
			timeout = time.After(self.settings.ValidationWaitTimeout)
		}
		self.authorizationHeldOnce.Do(func() {
			close(self.authorizationHeld)
		})
		ended := func() bool {
			defer unwatch()
			select {
			case <-done:
				return false
			case <-request.Context().Done():
			case <-self.stopping:
				answerProblem("Pebble is stopping")
			case <-timeout:
				fault := fmt.Sprintf(
					"Pebble's validation authority had not finished challenge %s %v after a request on its authorization arrived, which the hold answered 503 rather than let Pebble wedge",
					challengeId,
					self.settings.ValidationWaitTimeout,
				)
				self.logger.Print(fault)
				answerProblem(fault)
			}
			return true
		}()
		if ended {
			return false
		}
	}
	return true
}

// The challenges Pebble's validation authority finishes, read from its log, the only thing of Pebble's that tells
// when. The authority logs here with no prefix or flags, so its lines read as Pebble writes them; each line then goes
// on to the operator's writer through a logger of its own, with the operator's prefix and flags, so a line the
// operator's writer is slow to take slows the authority alone and not the front end's logging. Safe for concurrent
// use.
type pebbleValidations struct {
	logger *log.Logger

	stateLock sync.Mutex
	// by challenge id, only while a request watches it
	challengeIdWatches map[string]*pebbleValidationWatch
}

type pebbleValidationWatch struct {
	// closed once the validation authority has set the challenge valid or invalid
	done       chan struct{}
	watchCount int
}

func newPebbleValidations(logger *log.Logger) *pebbleValidations {
	return &pebbleValidations{
		logger:             logger,
		challengeIdWatches: map[string]*pebbleValidationWatch{},
	}
}

// done closes once the validation authority finishes challengeId from now on; a finish before the watch began closes
// nothing, so the caller reads the challenge's status after it. Each watch ends with its unwatch, which may be called
// more than once.
func (self *pebbleValidations) watch(challengeId string) (done <-chan struct{}, unwatch func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	watch, ok := self.challengeIdWatches[challengeId]
	if !ok {
		watch = &pebbleValidationWatch{done: make(chan struct{})}
		self.challengeIdWatches[challengeId] = watch
	}
	watch.watchCount += 1
	return watch.done, sync.OnceFunc(func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		watch.watchCount -= 1
		if watch.watchCount == 0 && self.challengeIdWatches[challengeId] == watch {
			delete(self.challengeIdWatches, challengeId)
		}
	})
}

// Lines of the validation authority's log; log.Logger writes each line whole.
func (self *pebbleValidations) Write(lines []byte) (int, error) {
	for _, line := range strings.Split(string(lines), "\n") {
		before, challengeId, found := strings.Cut(strings.TrimSpace(line), pebbleValidationDoneMarker)
		if !found || !strings.HasPrefix(before, "authz ") || challengeId == "" {
			continue
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if watch, ok := self.challengeIdWatches[challengeId]; ok {
				close(watch.done)
				delete(self.challengeIdWatches, challengeId)
			}
		}()
	}
	// after the watches it ends, so a held request goes however slow the operator's writer is
	self.logger.Print(string(lines))
	return len(lines), nil
}

// The stacks of Pebble's goroutines but its validation authorities' idle task loops, one of which every Pebble the
// process built keeps, as Pebble has no way to stop one.
func pebbleGoroutines() string {
	dump := &bytes.Buffer{}
	if err := pprof.Lookup("goroutine").WriteTo(dump, 2); err != nil {
		return err.Error()
	}
	stacks := []string{}
	for _, stack := range strings.Split(dump.String(), "\n\n") {
		if !strings.Contains(stack, "github.com/letsencrypt/pebble/") {
			continue
		}
		lines := strings.SplitN(stack, "\n", 3)
		if 2 <= len(lines) && strings.HasPrefix(lines[1], "github.com/letsencrypt/pebble/v2/va.VAImpl.processTasks(") {
			continue
		}
		stacks = append(stacks, stack)
	}
	return strings.Join(stacks, "\n\n")
}
