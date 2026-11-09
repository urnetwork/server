package model

// §7.8's revocation objects at api (owner decision 2; §2.8 evs-18): the writer
// of rev/<network_id>/<kind>/<subject_id>/_ on the first metadata cluster, and
// taskworker's deletion of each 20 minutes after its last write. The format and
// evs's and gkm's reader are the root's (sidecar/config_rev.go); the signature
// is oauth.SignRevocation's, which api and taskworker pass in, since model may
// not import oauth.
//
//	owe     WriteRevocation, in the caller's transaction: schedules a write
//	        task that retries until it lands, and after the commit tries the
//	        write at once. An object write cannot join the transaction, and a
//	        write before the commit would void a removal that then rolled back
//	write   GET the key; none: PUT If-None-Match; an object at or past this
//	        not_before: done; an earlier one: PUT If-Match its ETag to raise
//	        not_before, never lower it. A 412 or an unknown outcome reads again
//	sweep   LIST rev/ and DELETE each object whose last write is 20 minutes old,
//	        HEADing it again just before, so a raise that landed since the LIST
//	        stays
//
// rev/ writes are P0, never queued or shed (§11.13): with a write budget
// installed (SetWriteBudget) each PUT is issued to it as P0 of kind revocation,
// which never waits or refuses, and either way its bytes are counted. Holds no
// Postgres connection across store I/O.
//
// What a removal owes is the rows that record it, network_client.deactivate_time
// and sidecar_identity.revoke_time, which §19.10's restore re-applies and from
// which TEAM-M3-02 re-creates the live objects after a site loss: no table
// holds the objects themselves.
//
// One race stays: the store has no conditional DELETE (§11.3), so a raise that
// lands between the sweep's HEAD and its DELETE, on an object whose last write
// was 20 minutes before, is lost, and tokens issued between its old and new
// not_before stay honored until they expire. A raise needs a subject that
// outlived its first revocation, which a removed client does not.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Tunables of rev/'s writer and sweep.
type SidecarRevSettings struct {
	// Added to a revocation's time before it rounds up to a whole second: a
	// service token the exchange signs as the removal commits, from a read that
	// saw the subject live, carries an iat inside this margin and is void.
	// 2 s.
	NotBeforeMargin time.Duration
	// How long after its last write the sweep deletes an object: 20 minutes,
	// past a service token's 15 (§7.8's life row), so no token issued before
	// its not_before outlives it.
	DeleteAfter time.Duration
	// How many conditional writes one write tries before it hands the rest to
	// its task's retry. Each failure is another writer's raise landing first.
	WriteAttemptCount int
	// How many revocations one write task holds: a bulk removal owes one task
	// per this many objects.
	WriteTaskRevocationCount int
	// One write task's bound.
	WriteTaskMaxTime time.Duration
	// How long a write task that failed waits before it tries what is left.
	WriteRetryDelay time.Duration
	// Between two sweeps: a minute, so an object goes 20 to 21 minutes after
	// its last write.
	SweepInterval time.Duration
	// One sweep's bound.
	SweepMaxTime time.Duration
	// Keys one LIST asks for.
	SweepPageCount int
	// Pages one sweep reads before it stops: the next sweep starts over, and
	// objects deleted shrink the list it reads.
	SweepPageLimit int
	// The clock writes and the sweep read. nil is real time.
	Clock sidecar.Clock
}

func DefaultSidecarRevSettings() *SidecarRevSettings {
	return &SidecarRevSettings{
		NotBeforeMargin:          2 * time.Second,
		DeleteAfter:              20 * time.Minute,
		WriteAttemptCount:        8,
		WriteTaskRevocationCount: 256,
		WriteTaskMaxTime:         5 * time.Minute,
		WriteRetryDelay:          10 * time.Second,
		SweepInterval:            time.Minute,
		SweepMaxTime:             5 * time.Minute,
		SweepPageCount:           1000,
		SweepPageLimit:           100,
	}
}

var sidecarRevWriteCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "sidecar_rev",
	Name:      "writes_total",
	Help:      "rev/ object writes by outcome (§7.8): created, raised, current (an object at or past this not_before stood), failed",
}, []string{"outcome"})

var sidecarRevP0ByteCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "sidecar_rev",
	Name:      "p0_bytes_total",
	Help:      "Bytes of rev/ objects api and taskworker sent to the store, each try counted, P0 and never queued (§11.13)",
})

var sidecarRevDeleteCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "sidecar_rev",
	Name:      "deletes_total",
	Help:      "rev/ objects the sweep deleted 20 minutes after their last write (§7.8)",
})

func init() {
	prometheus.MustRegister(sidecarRevWriteCounter, sidecarRevP0ByteCounter, sidecarRevDeleteCounter)
}

// One object a removal owes: the subject's tokens issued before NotBefore are
// void.
type SidecarRevocation struct {
	NetworkId server.Id                `json:"network_id"`
	Kind      sidecar.RevocationKind   `json:"kind"`
	SubjectId server.Id                `json:"subject_id"`
	NotBefore time.Time                `json:"not_before"`
	Reason    sidecar.RevocationReason `json:"reason"`
}

// The object's key.
func (self *SidecarRevocation) key() string {
	return sidecar.RevocationKey(sidecar.Id(self.NetworkId), self.Kind, sidecar.Id(self.SubjectId))
}

// The not_before of a revocation at at: the whole second at or after at plus
// the margin.
func SidecarRevocationNotBefore(at time.Time, settings *SidecarRevSettings) time.Time {
	return sidecar.RevocationNotBefore(at.Add(settings.NotBeforeMargin))
}

// api's and taskworker's rev/ writer, on the first metadata cluster. Safe for
// concurrent use.
type SidecarRevStore struct {
	cluster  sidecar.BlobCluster
	sign     func(payload []byte) ([]byte, error)
	settings *SidecarRevSettings
	clock    sidecar.Clock

	stateLock sync.Mutex
	// the process's write budget, nil for none, and how it prices an object's
	// create (§11.13's cost rows)
	budget sidecar.WriteBudget
	price  func(objectByteCount int64) int64
}

// A writer on cluster, signing each object's payload with sign
// (oauth.SignRevocation). settings may be nil for the defaults.
func NewSidecarRevStore(cluster sidecar.BlobCluster, sign func(payload []byte) ([]byte, error), settings *SidecarRevSettings) *SidecarRevStore {
	if settings == nil {
		settings = DefaultSidecarRevSettings()
	}
	clock := settings.Clock
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	return &SidecarRevStore{
		cluster:  cluster,
		sign:     sign,
		settings: settings,
		clock:    clock,
	}
}

// Issues every rev/ write to budget before it is sent, as P0 of kind
// revocation, priced by price. OPS installs the process's budget with its first
// metadata cluster. nil removes it.
func (self *SidecarRevStore) SetWriteBudget(budget sidecar.WriteBudget, price func(objectByteCount int64) int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.budget, self.price = budget, price
}

// ctx for one PUT of byteCount bytes: counted, and issued to the budget as P0
// when one is installed.
func (self *SidecarRevStore) issueP0(ctx context.Context, byteCount int) context.Context {
	sidecarRevP0ByteCounter.Add(float64(byteCount))
	budget, price := func() (sidecar.WriteBudget, func(int64) int64) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.budget, self.price
	}()
	if budget == nil {
		return ctx
	}
	pricedByteCount := int64(byteCount)
	if price != nil {
		pricedByteCount = price(int64(byteCount))
	}
	return budget.IssueP0(ctx, self.cluster.ClusterId(), sidecar.WriteP0KindRevocation, pricedByteCount)
}

// This process's writer, which api and taskworker install once their first
// metadata cluster is wired (OPS's ServiceHost.Blobs); until then the post-commit
// write does nothing and each write task fails and retries, so an owed object is
// never dropped.
var sidecarRevStore atomic.Pointer[SidecarRevStore]

// Installs this process's writer; nil removes it.
func SetSidecarRevStore(store *SidecarRevStore) {
	sidecarRevStore.Store(store)
}

func currentSidecarRevStore() (*SidecarRevStore, error) {
	store := sidecarRevStore.Load()
	if store == nil {
		return nil, errors.New("this process holds no first metadata cluster for rev/")
	}
	return store, nil
}

// The payload an object holds, read without its signature: what this writer
// compares not_before against. A body that does not read is replaced.
func sidecarRevHeldRecord(body []byte) (*sidecar.RevocationRecord, bool) {
	parts := strings.Split(string(body), ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	record, err := sidecar.ParseRevocationPayload(payload)
	if err != nil {
		return nil, false
	}
	return record, true
}

// Called before each PUT of a write, with its key: the barrier a schedule holds
// a write at. nil in production.
var Testing_SidecarRevBeforePut func(key string)

// Lands one object at or past revocation's not_before: creates it, raises it,
// or finds it already there. An error leaves the outcome to the caller's retry,
// which reads again.
func (self *SidecarRevStore) Write(ctx context.Context, revocation *SidecarRevocation) error {
	key := revocation.key()
	notBefore := sidecar.RevocationNotBefore(revocation.NotBefore)
	for attempt := 0; attempt < self.settings.WriteAttemptCount; attempt += 1 {
		var etag string
		object, err := self.cluster.Get(ctx, key)
		switch {
		case errors.Is(err, sidecar.ErrBlobNotFound):
		case err != nil:
			sidecarRevWriteCounter.WithLabelValues("failed").Inc()
			return err
		default:
			body, readErr := io.ReadAll(io.LimitReader(object.Body, sidecar.RevocationObjectMaxByteCount+1))
			object.Body.Close()
			if readErr != nil {
				sidecarRevWriteCounter.WithLabelValues("failed").Inc()
				return readErr
			}
			etag = object.Etag
			if held, ok := sidecarRevHeldRecord(body); ok && !held.NotBefore.Before(notBefore) {
				// never lowered: an object at or past this not_before voids at least as much
				sidecarRevWriteCounter.WithLabelValues("current").Inc()
				return nil
			}
		}

		payload, err := sidecar.EncodeRevocationPayload(&sidecar.RevocationRecord{
			NetworkId: sidecar.Id(revocation.NetworkId),
			Kind:      revocation.Kind,
			SubjectId: sidecar.Id(revocation.SubjectId),
			NotBefore: notBefore,
			Reason:    revocation.Reason,
			Time:      self.clock.Now().UTC().Truncate(time.Second),
		})
		if err != nil {
			return err
		}
		signed, err := self.sign(payload)
		if err != nil {
			sidecarRevWriteCounter.WithLabelValues("failed").Inc()
			return err
		}
		if Testing_SidecarRevBeforePut != nil {
			Testing_SidecarRevBeforePut(key)
		}
		putCtx := self.issueP0(ctx, len(signed))
		if etag == "" {
			_, err = self.cluster.PutIfNoneMatch(putCtx, key, signed, nil)
		} else {
			_, err = self.cluster.PutIfMatch(putCtx, key, etag, signed, nil)
		}
		var unknown *sidecar.BlobUnknownError
		switch {
		case err == nil:
			if etag == "" {
				sidecarRevWriteCounter.WithLabelValues("created").Inc()
			} else {
				sidecarRevWriteCounter.WithLabelValues("raised").Inc()
			}
			return nil
		case errors.Is(err, sidecar.ErrBlobPrecondition), errors.As(err, &unknown):
			// another writer moved the key, or this PUT may have landed: the
			// next read decides (§11.8's unknown row, D-OPS-20)
			continue
		default:
			sidecarRevWriteCounter.WithLabelValues("failed").Inc()
			return err
		}
	}
	sidecarRevWriteCounter.WithLabelValues("failed").Inc()
	return fmt.Errorf("rev/ write did not land in %d attempts", self.settings.WriteAttemptCount)
}

// Deletes every object whose last write is DeleteAfter old, reading at most
// SweepPageLimit pages. Reports how many it deleted.
func (self *SidecarRevStore) Sweep(ctx context.Context) (deletedCount int, returnErr error) {
	expired := func(info *sidecar.BlobInfo) bool {
		return self.settings.DeleteAfter <= self.clock.Now().Sub(info.LastModified)
	}
	startAfter := ""
	for page := 0; page < self.settings.SweepPageLimit; page += 1 {
		objects, more, err := self.cluster.List(ctx, sidecar.RevocationKeyPrefix(), startAfter, self.settings.SweepPageCount)
		if err != nil {
			return deletedCount, err
		}
		for _, listed := range objects {
			startAfter = listed.Key
			if _, _, _, ok := sidecar.ParseRevocationKey(listed.Key); !ok || !expired(listed) {
				continue
			}
			// read again just before the delete: a raise since the LIST stays
			info, err := self.cluster.Head(ctx, listed.Key)
			if errors.Is(err, sidecar.ErrBlobNotFound) {
				continue
			}
			if err != nil {
				return deletedCount, err
			}
			if info.Etag != listed.Etag || !expired(info) {
				continue
			}
			if err := self.cluster.Delete(ctx, listed.Key); err != nil {
				return deletedCount, err
			}
			deletedCount += 1
			sidecarRevDeleteCounter.Inc()
		}
		if !more {
			break
		}
	}
	return deletedCount, nil
}

// Owes each revocation's object, in the caller's transaction: a write task,
// which retries until each lands, is scheduled with the transaction, and after
// the commit this process tries the writes at once. A transaction that rolls
// back owes nothing and writes nothing.
func WriteRevocations(ctx context.Context, tx server.PgTx, revocations []*SidecarRevocation) {
	if len(revocations) == 0 {
		return
	}
	settings := DefaultSidecarRevSettings()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	for start := 0; start < len(revocations); start += settings.WriteTaskRevocationCount {
		end := min(start+settings.WriteTaskRevocationCount, len(revocations))
		task.ScheduleTaskInTx(
			tx,
			WriteSidecarRevTask,
			&WriteSidecarRevArgs{Revocations: revocations[start:end]},
			owner,
			task.Priority(task.TaskPriorityFastest),
			task.MaxTime(settings.WriteTaskMaxTime),
		)
	}
	owed := append([]*SidecarRevocation{}, revocations...)
	server.AddTxPostCommit(tx, "sidecar_rev/"+server.NewId().String(), func() any {
		if Testing_SidecarRevSkipPostCommit != nil && Testing_SidecarRevSkipPostCommit() {
			return nil
		}
		store, err := currentSidecarRevStore()
		if err != nil {
			// the task lands them once a writer is installed
			return nil
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), server.TxPostCommitTimeout)
		defer cancel()
		for _, revocation := range owed {
			if err := store.Write(writeCtx, revocation); err != nil {
				if glog.V(1) {
					glog.Infof("[rev]post-commit write left to its task: %s\n", err)
				}
			}
		}
		return nil
	})
}

// One revocation's object, owed in the caller's transaction (WriteRevocations).
func WriteRevocation(ctx context.Context, tx server.PgTx, revocation *SidecarRevocation) {
	WriteRevocations(ctx, tx, []*SidecarRevocation{revocation})
}

// Whether the post-commit write is skipped, as a crash between the commit and
// the PUT would: the write task alone then lands the object. nil in production.
var Testing_SidecarRevSkipPostCommit func() bool

type WriteSidecarRevArgs struct {
	Revocations []*SidecarRevocation `json:"revocations"`
}

type WriteSidecarRevResult struct {
}

// Lands each owed object, retrying what failed after WriteRetryDelay with only
// what is left. Without this process's writer every object is left, and the
// task retries.
func WriteSidecarRevTask(
	writeSidecarRev *WriteSidecarRevArgs,
	clientSession *session.ClientSession,
) (*WriteSidecarRevResult, error) {
	settings := DefaultSidecarRevSettings()
	store, err := currentSidecarRevStore()
	if err != nil {
		return nil, task.WithRetryDelay(err, settings.WriteRetryDelay)
	}
	left := []*SidecarRevocation{}
	var writeErr error
	for _, revocation := range writeSidecarRev.Revocations {
		if err := store.Write(clientSession.Ctx, revocation); err != nil {
			left = append(left, revocation)
			writeErr = errors.Join(writeErr, err)
		}
	}
	if 0 < len(left) {
		return nil, task.WithRetryDelayAndArgs(
			fmt.Errorf("%d of %d rev/ writes did not land: %w", len(left), len(writeSidecarRev.Revocations), writeErr),
			settings.WriteRetryDelay,
			&WriteSidecarRevArgs{Revocations: left},
		)
	}
	return &WriteSidecarRevResult{}, nil
}

func WriteSidecarRevTaskPost(
	writeSidecarRev *WriteSidecarRevArgs,
	writeSidecarRevResult *WriteSidecarRevResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}

// One sweep by this process's writer: what taskworker's rev/ deletion runs. A
// process with no writer deletes nothing, so an object outlives its 20 minutes
// rather than going early.
func SweepSidecarRevocations(ctx context.Context) (int, error) {
	store, err := currentSidecarRevStore()
	if err != nil {
		return 0, err
	}
	return store.Sweep(ctx)
}
