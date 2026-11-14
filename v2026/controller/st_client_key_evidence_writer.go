// Client-key registration evidence reaches remote public storage through one
// bounded, backed-off, asynchronous writer per registration owner.
//
// On 2026-11-01 these writes ran inside each ClientKey control frame, and the
// MinIO SDK retried every refusal up to ten times 0.2-1 s apart. They held
// edge-5's request pool at its ceiling (about 3,500 PutObject/s, 93% answered
// 429), so MinIO refused every other client too.
//
// A registration is complete once its signed generation is committed in
// Postgres, which retains the exact wrapper bytes. Storage holds the public
// copy that GET /sn/evidence serves by content hash, and nothing needs it at
// registration time: the frame's success tells the client its key is
// registered, and providers read that key from Postgres (/key and contract
// metadata). Validators receive the wrappers, and so their hashes, only from
// the observation routes, which create and read back every history record
// before they respond; an unchanged key's next registration hands its
// evidence to this writer again. So the frame does not wait for storage, and
// a publication this writer gives up on is counted, not lost.
package controller

import (
	"container/heap"
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/startifact"
)

// Final outcomes, one per handed-over publication, and attempt results.
const (
	// created and read back exactly
	stClientKeyEvidenceOutcomeOk = "ok"
	// already stored with these exact bytes (a refused conditional create or a
	// verified read), verified by this writer within its memory, or joined an
	// identical pending publication whose own outcome is counted
	stClientKeyEvidenceOutcomeExists = "exists"
	// gave up after its attempt budget; the last attempt was throttled
	stClientKeyEvidenceOutcomeThrottled = "throttled"
	// gave up on any other failure, or on an answer that will not change
	stClientKeyEvidenceOutcomeError = "error"
	// not admitted: the bounded queue was full, or the writer was closed
	stClientKeyEvidenceOutcomeDropped = "dropped"
)

type stClientKeyEvidenceWriterSettings struct {
	// Concurrent publications. Each attempt sends one request at a time (a
	// conditional create or a read, then the exact readback), so this is the
	// writer's bound on MinIO's request pool.
	Workers int
	// Publications pending (queued, in flight or waiting out a backoff).
	QueueCapacity int
	// Wrapper bytes pending.
	QueueBytes int
	// Storage attempts per publication, including the first.
	MaxAttempts int
	// Full-jitter backoff after the n-th consecutive failure: uniform in
	// [MinBackoff, min(MaxBackoff, MinBackoff*2^n)].
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// Content verified by this writer, remembered so a repeat sends nothing.
	KnownCapacity int
	KnownTtl      time.Duration
	// Bounds one attempt. It is the frame's former bound on the same requests.
	AttemptTimeout time.Duration
}

// Default Workers is server.DefaultClientKeyEvidenceConcurrency (4): the
// fleet's 62 writers hold at most 248 of edge-5's 1,024 request slots. The
// queue holds about 20 minutes of one connect process's normal publications
// (fleet-wide 100-200 per second in October); a refused 1.5 KiB wrapper costs
// nothing to keep. Eight attempts back off for about two minutes in all.
func defaultStClientKeyEvidenceWriterSettings() stClientKeyEvidenceWriterSettings {
	return stClientKeyEvidenceWriterSettings{
		Workers:        server.DefaultClientKeyEvidenceConcurrency,
		QueueCapacity:  8192,
		QueueBytes:     32 * 1024 * 1024,
		MaxAttempts:    8,
		MinBackoff:     time.Second,
		MaxBackoff:     30 * time.Second,
		KnownCapacity:  16384,
		KnownTtl:       time.Hour,
		AttemptTimeout: stCallTimeout,
	}
}

// The process's writer settings: the defaults, with the concurrency from
// minio.yml (`client_key_evidence_concurrency`).
func stClientKeyEvidenceWriterSettingsFromConfig() stClientKeyEvidenceWriterSettings {
	settings := defaultStClientKeyEvidenceWriterSettings()
	if config, present := server.LoadBlobStoreConfig(); present && 0 < config.ClientKeyEvidenceConcurrency {
		settings.Workers = config.ClientKeyEvidenceConcurrency
	}
	return settings
}

// Fixed-label collectors. Gauges move by deltas, so writers sum.
type stClientKeyEvidenceMetrics struct {
	publications       *prometheus.CounterVec
	attempts           *prometheus.CounterVec
	queueDepth         prometheus.Gauge
	inflight           prometheus.Gauge
	attemptSeconds     prometheus.Histogram
	publicationSeconds prometheus.Histogram
}

func newStClientKeyEvidenceMetrics() *stClientKeyEvidenceMetrics {
	metrics := &stClientKeyEvidenceMetrics{
		publications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_client_key_evidence_publications_total",
			Help: "Client-key evidence publications by final outcome: ok (created and read back), exists (already stored with these bytes, verified recently, or joined an identical pending publication), throttled (attempt budget spent, last refusal 429/503/SlowDown), error (any other failure), dropped (queue full)",
		}, []string{"outcome"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_client_key_evidence_attempts_total",
			Help: "Client-key evidence storage attempts by result: ok (created), exists (identical bytes already stored), throttled (429/503/SlowDown), error",
		}, []string{"result"}),
		queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "urnetwork_client_key_evidence_queue_depth",
			Help: "Client-key evidence publications not yet finished: queued, in flight or waiting out a backoff",
		}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "urnetwork_client_key_evidence_inflight",
			Help: "Client-key evidence storage attempts in flight",
		}),
		attemptSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "urnetwork_client_key_evidence_attempt_seconds",
			Help:    "One client-key evidence storage attempt, every request it sends included",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300},
		}),
		publicationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "urnetwork_client_key_evidence_publication_seconds",
			Help:    "From admission to the final outcome of an admitted client-key evidence publication, queueing and backoff included",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
		}),
	}
	for _, outcome := range []string{stClientKeyEvidenceOutcomeOk, stClientKeyEvidenceOutcomeExists, stClientKeyEvidenceOutcomeThrottled, stClientKeyEvidenceOutcomeError, stClientKeyEvidenceOutcomeDropped} {
		metrics.publications.WithLabelValues(outcome)
	}
	for _, result := range []string{stClientKeyEvidenceOutcomeOk, stClientKeyEvidenceOutcomeExists, stClientKeyEvidenceOutcomeThrottled, stClientKeyEvidenceOutcomeError} {
		metrics.attempts.WithLabelValues(result)
	}
	return metrics
}

var defaultStClientKeyEvidenceMetrics = newStClientKeyEvidenceMetrics()

func init() {
	prometheus.MustRegister(
		defaultStClientKeyEvidenceMetrics.publications,
		defaultStClientKeyEvidenceMetrics.attempts,
		defaultStClientKeyEvidenceMetrics.queueDepth,
		defaultStClientKeyEvidenceMetrics.inflight,
		defaultStClientKeyEvidenceMetrics.attemptSeconds,
		defaultStClientKeyEvidenceMetrics.publicationSeconds,
	)
}

// One object in one bucket at one endpoint.
type stClientKeyEvidenceKey [sha256.Size]byte

func newStClientKeyEvidenceKey(store server.BlobStore, contentKey string) stClientKeyEvidenceKey {
	return stClientKeyEvidenceKey(sha256.Sum256([]byte(store.Authority() + "\x00" + store.Bucket() + "\x00" + contentKey)))
}

// One admitted publication. The writer's lock guards its mutable fields.
type stClientKeyEvidencePublication struct {
	key      stClientKeyEvidenceKey
	store    server.BlobStore
	evidence *startifact.PreparedClientKeyEvidence
	// read first, create only when absent
	verifyFirst bool
	admittedAt  time.Time
	attempts    int
	// the earliest next attempt while waiting out a backoff
	due time.Time
	// the position in the waiting heap
	index int
}

// Publications waiting out a backoff, earliest due first.
type stClientKeyEvidenceWaiting []*stClientKeyEvidencePublication

func (self stClientKeyEvidenceWaiting) Len() int           { return len(self) }
func (self stClientKeyEvidenceWaiting) Less(i, j int) bool { return self[i].due.Before(self[j].due) }
func (self stClientKeyEvidenceWaiting) Swap(i, j int) {
	self[i], self[j] = self[j], self[i]
	self[i].index = i
	self[j].index = j
}
func (self *stClientKeyEvidenceWaiting) Push(value any) {
	publication := value.(*stClientKeyEvidencePublication)
	publication.index = len(*self)
	*self = append(*self, publication)
}
func (self *stClientKeyEvidenceWaiting) Pop() any {
	old := *self
	publication := old[len(old)-1]
	old[len(old)-1] = nil
	*self = old[:len(old)-1]
	publication.index = -1
	return publication
}

// Verified content, least recently verified evicted first. Not concurrent;
// the writer's lock guards it.
type stClientKeyEvidenceKnown struct {
	capacity int
	ttl      time.Duration
	order    *list.List
	elements map[stClientKeyEvidenceKey]*list.Element
}

type stClientKeyEvidenceKnownEntry struct {
	key        stClientKeyEvidenceKey
	verifiedAt time.Time
}

func newStClientKeyEvidenceKnown(capacity int, ttl time.Duration) *stClientKeyEvidenceKnown {
	return &stClientKeyEvidenceKnown{capacity: capacity, ttl: ttl, order: list.New(), elements: map[stClientKeyEvidenceKey]*list.Element{}}
}

// Whether key was verified within the TTL. An expired entry is removed.
func (self *stClientKeyEvidenceKnown) contains(key stClientKeyEvidenceKey, now time.Time) bool {
	element, ok := self.elements[key]
	if !ok {
		return false
	}
	if self.ttl <= now.Sub(element.Value.(*stClientKeyEvidenceKnownEntry).verifiedAt) {
		self.order.Remove(element)
		delete(self.elements, key)
		return false
	}
	return true
}

func (self *stClientKeyEvidenceKnown) add(key stClientKeyEvidenceKey, now time.Time) {
	if self.capacity <= 0 {
		return
	}
	if element, ok := self.elements[key]; ok {
		element.Value.(*stClientKeyEvidenceKnownEntry).verifiedAt = now
		self.order.MoveToFront(element)
		return
	}
	self.elements[key] = self.order.PushFront(&stClientKeyEvidenceKnownEntry{key: key, verifiedAt: now})
	for self.capacity < self.order.Len() {
		oldest := self.order.Back()
		self.order.Remove(oldest)
		delete(self.elements, oldest.Value.(*stClientKeyEvidenceKnownEntry).key)
	}
}

func (self *stClientKeyEvidenceKnown) len() int {
	return self.order.Len()
}

// The writer. One dispatcher goroutine runs while anything is pending and
// exits at quiescence; at most Workers attempt goroutines run at once. After
// any failed attempt the whole writer pauses for a jittered backoff that
// grows with consecutive failures, so throttling slows every publication, not
// only the refused one, and a success resets it. Safe for concurrent use.
type stClientKeyEvidenceWriter struct {
	settings stClientKeyEvidenceWriterSettings
	metrics  *stClientKeyEvidenceMetrics
	ctx      context.Context
	cancel   context.CancelFunc
	// signaled on admission and on every attempt's completion
	wake chan struct{}

	stateLock    sync.Mutex
	ready        []*stClientKeyEvidencePublication
	waiting      stClientKeyEvidenceWaiting
	pending      map[stClientKeyEvidenceKey]*stClientKeyEvidencePublication
	pendingBytes int
	inflight     int
	running      bool
	closed       bool
	known        *stClientKeyEvidenceKnown
	pausedUntil  time.Time
	failures     int
	// the backoff jitter, a connect.NewSecureRand generator
	random *rand.Rand
	// closed while nothing is pending
	idle chan struct{}

	// Test seams observe scheduling; none supplies a storage result.
	now                 func() time.Time
	afterOutcomeForTest func(contentHash string, outcome string)
	afterBackoffForTest func(failures int, delay time.Duration)
}

func newStClientKeyEvidenceWriter(settings stClientKeyEvidenceWriterSettings, metrics *stClientKeyEvidenceMetrics) *stClientKeyEvidenceWriter {
	settings.Workers = max(1, settings.Workers)
	settings.QueueCapacity = max(1, settings.QueueCapacity)
	settings.MaxAttempts = max(1, settings.MaxAttempts)
	settings.MinBackoff = max(time.Millisecond, settings.MinBackoff)
	settings.MaxBackoff = max(settings.MinBackoff, settings.MaxBackoff)
	settings.QueueBytes = max(startifact.MaxClientKeyEvidenceBytes, settings.QueueBytes)
	if settings.AttemptTimeout <= 0 {
		settings.AttemptTimeout = stCallTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &stClientKeyEvidenceWriter{
		settings: settings,
		metrics:  metrics,
		ctx:      ctx,
		cancel:   cancel,
		wake:     make(chan struct{}, 1),
		pending:  map[stClientKeyEvidenceKey]*stClientKeyEvidencePublication{},
		known:    newStClientKeyEvidenceKnown(settings.KnownCapacity, settings.KnownTtl),
		random:   connect.NewSecureRand(),
		idle:     idle,
		now:      time.Now,
	}
}

func (self *stClientKeyEvidenceWriter) signal() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// Hands one retained wrapper to the writer without waiting for storage.
// verifyFirst reads the object before any write, for evidence that is almost
// certainly stored already. A nil result means the evidence is stored,
// pending, or counted as dropped; only an invalid wrapper is an error.
func (self *stClientKeyEvidenceWriter) enqueue(store server.BlobStore, encoded []byte, contentHash string, verifyFirst bool) error {
	if store == nil {
		return errors.New("client-key evidence store is unavailable")
	}
	evidence, err := startifact.PrepareClientKeyEvidence(encoded, contentHash)
	if err != nil {
		return err
	}
	contentKey, err := evidence.ContentKey(store)
	if err != nil {
		return err
	}
	key := newStClientKeyEvidenceKey(store, contentKey)
	outcome := ""
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		now := self.now()
		if self.closed {
			outcome = stClientKeyEvidenceOutcomeDropped
			return
		}
		if self.known.contains(key, now) {
			outcome = stClientKeyEvidenceOutcomeExists
			return
		}
		if _, ok := self.pending[key]; ok {
			outcome = stClientKeyEvidenceOutcomeExists
			return
		}
		if self.settings.QueueCapacity <= len(self.pending) || self.settings.QueueBytes < self.pendingBytes+evidence.Size() {
			outcome = stClientKeyEvidenceOutcomeDropped
			return
		}
		publication := &stClientKeyEvidencePublication{key: key, store: store, evidence: evidence, verifyFirst: verifyFirst, admittedAt: now, index: -1}
		if len(self.pending) == 0 {
			self.idle = make(chan struct{})
		}
		self.pending[key] = publication
		self.pendingBytes += evidence.Size()
		self.ready = append(self.ready, publication)
		self.metrics.queueDepth.Inc()
		if !self.running {
			self.running = true
			go self.run()
		}
	}()
	if outcome != "" {
		self.metrics.publications.WithLabelValues(outcome).Inc()
		if outcome == stClientKeyEvidenceOutcomeDropped && glog.V(1) {
			glog.Infof("[ckev]%s dropped: the writer is closed or its queue is full\n", contentHash)
		}
		if self.afterOutcomeForTest != nil {
			self.afterOutcomeForTest(contentHash, outcome)
		}
		return nil
	}
	self.signal()
	return nil
}

// Schedules ready publications within the concurrency bound and the pause.
func (self *stClientKeyEvidenceWriter) run() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		var launch []*stClientKeyEvidencePublication
		wait := time.Duration(-1)
		exit := false
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			now := self.now()
			for 0 < self.waiting.Len() && !self.waiting[0].due.After(now) {
				self.ready = append(self.ready, heap.Pop(&self.waiting).(*stClientKeyEvidencePublication))
			}
			if now.Before(self.pausedUntil) {
				wait = self.pausedUntil.Sub(now)
			} else {
				for self.inflight < self.settings.Workers && 0 < len(self.ready) {
					publication := self.ready[0]
					self.ready[0] = nil
					self.ready = self.ready[1:]
					self.inflight++
					launch = append(launch, publication)
				}
				if len(self.ready) == 0 {
					self.ready = nil
				}
				if 0 < self.waiting.Len() && len(self.ready) == 0 {
					wait = self.waiting[0].due.Sub(now)
				}
			}
			if len(launch) == 0 && self.inflight == 0 && len(self.ready) == 0 && self.waiting.Len() == 0 {
				self.running = false
				exit = true
			}
		}()
		if exit {
			return
		}
		for _, publication := range launch {
			self.metrics.inflight.Inc()
			go self.attempt(publication)
		}
		if wait < 0 {
			<-self.wake
			continue
		}
		timer.Reset(wait)
		select {
		case <-self.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// One storage attempt, then its accounting and scheduling.
func (self *stClientKeyEvidenceWriter) attempt(publication *stClientKeyEvidencePublication) {
	started := self.now()
	created, err := self.send(publication)
	result := stClientKeyEvidenceOutcomeOk
	switch {
	case err == nil && !created:
		result = stClientKeyEvidenceOutcomeExists
	case err != nil && server.IsBlobThrottled(err):
		result = stClientKeyEvidenceOutcomeThrottled
	case err != nil:
		result = stClientKeyEvidenceOutcomeError
	}
	self.metrics.inflight.Dec()
	self.metrics.attempts.WithLabelValues(result).Inc()
	self.metrics.attemptSeconds.Observe(self.now().Sub(started).Seconds())
	outcome := ""
	var pauseFailures int
	var pause time.Duration
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		now := self.now()
		self.inflight--
		publication.attempts++
		if err == nil {
			self.failures = 0
			self.known.add(publication.key, now)
			outcome = result
			self.finishWithLock(publication)
			return
		}
		self.failures++
		pauseFailures, pause = self.failures, self.backoffWithLock(self.failures)
		if pausedUntil := now.Add(pause); self.pausedUntil.Before(pausedUntil) {
			self.pausedUntil = pausedUntil
		}
		switch {
		case self.closed:
			outcome = stClientKeyEvidenceOutcomeDropped
		case server.IsBlobRequestRefused(err) || isStClientKeyEvidenceConflict(err) || self.settings.MaxAttempts <= publication.attempts:
			outcome = result
		default:
			publication.due = now.Add(self.backoffWithLock(publication.attempts))
			heap.Push(&self.waiting, publication)
			return
		}
		self.finishWithLock(publication)
	}()
	if 0 < pause && self.afterBackoffForTest != nil {
		self.afterBackoffForTest(pauseFailures, pause)
	}
	if outcome != "" {
		self.metrics.publications.WithLabelValues(outcome).Inc()
		self.metrics.publicationSeconds.Observe(self.now().Sub(publication.admittedAt).Seconds())
		if err != nil && glog.V(1) {
			glog.Infof("[ckev]%s %s after %d attempts: %s\n", publication.evidence.ContentHash(), outcome, publication.attempts, err)
		}
		if self.afterOutcomeForTest != nil {
			self.afterOutcomeForTest(publication.evidence.ContentHash(), outcome)
		}
	}
	self.signal()
}

// The storage requests of one attempt, each sent once.
func (self *stClientKeyEvidenceWriter) send(publication *stClientKeyEvidencePublication) (created bool, resultErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			created, resultErr = false, fmt.Errorf("client-key evidence attempt panicked: %v", recovered)
		}
	}()
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.AttemptTimeout)
	defer cancel()
	store := server.SingleAttemptBlobStore(publication.store)
	if publication.verifyFirst {
		return publication.evidence.VerifyOrPublish(ctx, store)
	}
	return publication.evidence.Publish(ctx, store)
}

// Removes a publication with its final outcome.
func (self *stClientKeyEvidenceWriter) finishWithLock(publication *stClientKeyEvidencePublication) {
	if self.pending[publication.key] != publication {
		return
	}
	delete(self.pending, publication.key)
	self.pendingBytes -= publication.evidence.Size()
	self.metrics.queueDepth.Dec()
	if len(self.pending) == 0 {
		close(self.idle)
	}
}

// The jittered delay after the n-th consecutive failure (n >= 1): uniform in
// [MinBackoff, min(MaxBackoff, MinBackoff*2^n)], so 1-2 s, 1-4 s, ... 1-30 s.
func (self *stClientKeyEvidenceWriter) backoffWithLock(failures int) time.Duration {
	ceiling := self.settings.MinBackoff
	for range max(1, failures) {
		if self.settings.MaxBackoff/2 < ceiling {
			ceiling = self.settings.MaxBackoff
			break
		}
		ceiling *= 2
	}
	ceiling = min(ceiling, self.settings.MaxBackoff)
	if ceiling <= self.settings.MinBackoff {
		return self.settings.MinBackoff
	}
	return self.settings.MinBackoff + time.Duration(self.random.Int64N(int64(ceiling-self.settings.MinBackoff)+1))
}

// A conflicting stored object is permanent: retrying cannot change it.
func isStClientKeyEvidenceConflict(err error) bool {
	var conflict *startifact.ImmutableConflictError
	return errors.As(err, &conflict)
}

// Waits until nothing is pending.
func (self *stClientKeyEvidenceWriter) waitIdle(ctx context.Context) error {
	idle := func() chan struct{} {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.idle
	}()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stops admitting and abandons pending publications, counted as dropped.
// Attempts in flight are canceled and also counted as dropped.
func (self *stClientKeyEvidenceWriter) close() {
	var dropped []*stClientKeyEvidencePublication
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return
		}
		self.closed = true
		dropped = append(dropped, self.ready...)
		self.ready = nil
		for 0 < self.waiting.Len() {
			dropped = append(dropped, heap.Pop(&self.waiting).(*stClientKeyEvidencePublication))
		}
		for _, publication := range dropped {
			self.finishWithLock(publication)
		}
	}()
	self.cancel()
	for _, publication := range dropped {
		self.metrics.publications.WithLabelValues(stClientKeyEvidenceOutcomeDropped).Inc()
		if self.afterOutcomeForTest != nil {
			self.afterOutcomeForTest(publication.evidence.ContentHash(), stClientKeyEvidenceOutcomeDropped)
		}
	}
	self.signal()
}
