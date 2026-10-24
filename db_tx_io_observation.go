// A test-only observer of the work that leaves a PostgreSQL transaction idle
// on its server connection. Under transaction pooling a transaction holds a
// pooler server from BEGIN to COMMIT, so a Redis call, or a second pooled
// connection, made by the goroutine that owns an open transaction keeps that
// server idle for the whole wait. The observer records such I/O, and the delay
// from BEGIN to the transaction's first statement, naming the first caller
// frames outside this package. Production never installs one: a transaction,
// a statement, a pooled connection and a Redis callback then each cost one
// atomic load.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"
)

type TxIoKind string

const (
	// a Redis callback ran while its goroutine owned an open transaction
	TxIoRedis TxIoKind = "redis"
	// a pooled connection was acquired while its goroutine owned an open transaction
	TxIoNestedDb TxIoKind = "nested_db"
)

// I/O issued while a transaction was open. TxSite names the transaction's
// owner and Site the code that issued the I/O, innermost frame first.
type TxIoEvent struct {
	Kind   TxIoKind
	TxSite string
	Site   string
}

// The delay from a completed BEGIN to the transaction's first statement, which
// the pooler's server spends idle in transaction.
type TxIoFirstStatement struct {
	TxSite string
	Site   string
	Delay  time.Duration
}

type TxIoObservation struct {
	stateLock       sync.Mutex
	goroutineOpens  map[uint64][]*txIoOpen
	txOpens         map[*postCommitPgTx]*txIoOpen
	events          []TxIoEvent
	firstStatements []TxIoFirstStatement
}

type txIoOpen struct {
	site      string
	begun     time.Time
	statement bool
}

var txIoObservation atomic.Pointer[TxIoObservation]

// The test environment's inventory of every test's transactions
// (Testing_AuditTxIo). It never excludes a test's own observation.
var txIoAuditObservation atomic.Pointer[TxIoObservation]

func txIoObservations() [2]*TxIoObservation {
	return [2]*TxIoObservation{txIoObservation.Load(), txIoAuditObservation.Load()}
}

// Observes every transaction in this process until the returned function
// runs. Tests only; one observer at a time.
func Testing_ObserveTxIo() (*TxIoObservation, func()) {
	observation := &TxIoObservation{
		goroutineOpens: map[uint64][]*txIoOpen{},
		txOpens:        map[*postCommitPgTx]*txIoOpen{},
	}
	if !txIoObservation.CompareAndSwap(nil, observation) {
		panic("a transaction I/O observation is already installed")
	}
	return observation, func() {
		txIoObservation.CompareAndSwap(observation, nil)
	}
}

// Observes every transaction in this process alongside any test's own
// observation, until the returned function runs. The test environment
// installs it around each test when WARP_TEST_TX_IO_AUDIT is set.
func Testing_AuditTxIo() (*TxIoObservation, func()) {
	observation := &TxIoObservation{
		goroutineOpens: map[uint64][]*txIoOpen{},
		txOpens:        map[*postCommitPgTx]*txIoOpen{},
	}
	if !txIoAuditObservation.CompareAndSwap(nil, observation) {
		panic("a transaction I/O audit is already installed")
	}
	return observation, func() {
		txIoAuditObservation.CompareAndSwap(observation, nil)
	}
}

func (self *TxIoObservation) Events() []TxIoEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

func (self *TxIoObservation) FirstStatements() []TxIoFirstStatement {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.firstStatements)
}

// Registers tx as open on the calling goroutine, which owns it until the
// returned function runs.
func (self *TxIoObservation) begin(tx *postCommitPgTx) func() {
	goroutineId := currentGoroutineId()
	open := &txIoOpen{site: txIoCallerSite(), begun: time.Now()}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.goroutineOpens[goroutineId] = append(self.goroutineOpens[goroutineId], open)
	self.txOpens[tx] = open
	return func() {
		self.end(goroutineId, tx, open)
	}
}

func (self *TxIoObservation) end(goroutineId uint64, tx *postCommitPgTx, open *txIoOpen) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	opens := slices.DeleteFunc(self.goroutineOpens[goroutineId], func(other *txIoOpen) bool {
		return other == open
	})
	if len(opens) == 0 {
		delete(self.goroutineOpens, goroutineId)
	} else {
		self.goroutineOpens[goroutineId] = opens
	}
	delete(self.txOpens, tx)
}

// Records the delay before the first statement of an observed transaction.
func (self *TxIoObservation) statement(tx *postCommitPgTx) {
	now := time.Now()
	open, first := func() (*txIoOpen, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		open := self.txOpens[tx]
		if open == nil || open.statement {
			return nil, false
		}
		open.statement = true
		return open, true
	}()
	if !first {
		return
	}
	site := txIoCallerSite()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.firstStatements = append(self.firstStatements, TxIoFirstStatement{
		TxSite: open.site,
		Site:   site,
		Delay:  now.Sub(open.begun),
	})
}

// Records I/O of kind when the calling goroutine owns an open transaction.
func (self *TxIoObservation) io(kind TxIoKind) {
	goroutineId := currentGoroutineId()
	txSite, open := func() (string, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		opens := self.goroutineOpens[goroutineId]
		if len(opens) == 0 {
			return "", false
		}
		return opens[len(opens)-1].site, true
	}()
	if !open {
		return
	}
	site := txIoCallerSite()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, TxIoEvent{Kind: kind, TxSite: txSite, Site: site})
}

func observeTxIo(kind TxIoKind) {
	for _, observation := range txIoObservations() {
		if observation != nil {
			observation.io(kind)
		}
	}
}

// Registers tx with each installed observation; the returned function ends
// every registration.
func observeTxIoBegin(tx *postCommitPgTx) func() {
	var ends []func()
	for _, observation := range txIoObservations() {
		if observation != nil {
			ends = append(ends, observation.begin(tx))
		}
	}
	return func() {
		for _, end := range ends {
			end()
		}
	}
}

func observeTxIoStatement(tx *postCommitPgTx) {
	for _, observation := range txIoObservations() {
		if observation != nil {
			observation.statement(tx)
		}
	}
}

// With WARP_TEST_TX_IO_AUDIT naming a directory, the test environment
// audits each test attempt's transactions and appends one JSON line per
// attempt to a file per test process there. Otherwise it does nothing.
func testTxIoAudit(test string) func() {
	directory := os.Getenv("WARP_TEST_TX_IO_AUDIT")
	if directory == "" {
		return func() {}
	}
	observation, uninstall := Testing_AuditTxIo()
	return func() {
		uninstall()
		line, err := json.Marshal(struct {
			Test            string
			Events          []TxIoEvent
			FirstStatements []TxIoFirstStatement
		}{test, observation.Events(), observation.FirstStatements()})
		if err == nil {
			var file *os.File
			file, err = os.OpenFile(filepath.Join(directory, fmt.Sprintf("tx-io-%d.jsonl", os.Getpid())), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, err = file.Write(append(line, '\n'))
				err = errors.Join(err, file.Close())
			}
		}
		if err != nil {
			glog.Errorf("[test_env]transaction I/O audit not written: %s\n", err)
		}
	}
}

// The runtime prints the id first in every goroutine header. Tests only.
func currentGoroutineId() uint64 {
	var buffer [64]byte
	header := bytes.TrimPrefix(buffer[:runtime.Stack(buffer[:], false)], []byte("goroutine "))
	if end := bytes.IndexByte(header, ' '); 0 <= end {
		header = header[:end]
	}
	id, _ := strconv.ParseUint(string(header), 10, 64)
	return id
}

// The innermost three frames outside this package's own sources, innermost
// first. This package's tests are callers too.
func txIoCallerSite() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	sites := []string{}
	for len(sites) < 3 {
		frame, more := frames.Next()
		internal := strings.HasPrefix(frame.Function, "github.com/urnetwork/server.") && !strings.HasSuffix(frame.File, "_test.go")
		if frame.Function != "" && !internal && !strings.HasPrefix(frame.Function, "runtime.") {
			function := frame.Function[strings.LastIndex(frame.Function, "/")+1:]
			sites = append(sites, fmt.Sprintf("%s (%s:%d)", function, filepath.Base(frame.File), frame.Line))
		}
		if !more {
			break
		}
	}
	return strings.Join(sites, " < ")
}
