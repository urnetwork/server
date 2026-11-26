package model

// §7.8's revocation objects at api (owner decision 2; §2.8 evs-18): the writer
// of rev/<network_id>/<kind>/<subject_id>/_ on the first metadata cluster, and
// taskworker's deletion of each 20 minutes after its last write. The format and
// the reader evs, gkm and sockets' block use (sockets' since QD-1029) are the
// root's (sidecar/config_rev.go); the signature is oauth.SignRevocation's, which
// the API issuer provides. The taskworker holds only the expiry sweep, and a
// process that runs both, all's, writes and sweeps through the issuer's
// (InstallSidecarRevStore).
//
//	owe     WriteRevocation, in the caller's transaction: schedules a write
//	        task that retries until it lands, and after the commit tries the
//	        write at once. An object write cannot join the transaction, and a
//	        write before the commit would void a removal that then rolled back
//	write   GET the key; none: PUT If-None-Match; an object at or past this
//	        not_before: done; an earlier one: PUT If-Match its ETag to raise
//	        not_before, never lower it. A 412 or an unknown outcome reads again,
//	        and a read that finds this write's own bytes after an unknown outcome
//	        is its create or raise landed, its answer lost
//	sweep   LIST rev/ and DELETE each object whose last write is 20 minutes old,
//	        HEADing it again just before, so a raise that landed since the LIST
//	        stays
//
// rev/ writes are P0, never queued or shed (§11.13; QD-084): with a write
// budget installed (SetWriteBudget) each PUT and the sweep's each DELETE carry
// P0, and each is spent once as P0 of kind revocation, which never waits or
// refuses, once the store shows it took effect, at its own one of §11.13's
// cost rows (api_write_spend.go): a create at the create row, a raise, an
// If-Match PUT over the object, at the overwrite row, and the sweep's DELETE
// at the delete row. A 412, and a PUT or DELETE a read shows never landed,
// spend nothing, and one whose outcome stays unknown is spent as issued, once.
// An object this write made keeps a not_before at or past its own until the
// sweep, since no writer lowers one, so after an unknown outcome a read that
// finds no object, or another's at or below this not_before, shows the PUT
// never landed; after the sweep's, the object it meant standing shows its
// DELETE never landed. Either way the bytes sent are counted. Holds no
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
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

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
	// The most bytes of a standing object a write reads before it compares
	// not_before: the format's bound, sidecar.RevocationObjectMaxByteCount,
	// which only a format change moves, since a write that reads less than an
	// object holds replaces it and could lower its not_before. Non-positive
	// refuses every write.
	ObjectMaxByteCount int
	// How many PUTs and sweep DELETEs whose outcome stayed unknown, the read
	// after it failing too, the store remembers by key: each is spent as issued,
	// once, and the write of its key that settles it spends only what its own
	// cost row prices past that (api_write_spend.go). Past it such a write is
	// spent and not remembered, so the write that settles it spends again, one
	// write over. 1,024: each needs the store to fail both a write and the read
	// after it. Non-positive remembers none.
	UnsettledWriteCount int
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
		ObjectMaxByteCount:       sidecar.RevocationObjectMaxByteCount,
		UnsettledWriteCount:      1024,
	}
}

var sidecarRevWriteCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "sidecar_rev",
	Name:      "writes_total",
	Help:      "rev/ object writes by outcome (§7.8): created and raised, each create or raise that took effect, a lost answer's once a read finds its bytes; current (an object at or past this not_before stood); failed",
}, []string{"outcome"})

var sidecarRevP0ByteCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "sidecar_rev",
	Name:      "p0_bytes_total",
	Help:      "Bytes of rev/ objects the API issuer sent to the store, each try counted, P0 and never queued (§11.13)",
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

// The issuer's writer, or the worker's expiry sweep, on the first metadata cluster. Safe for
// concurrent use.
type SidecarRevStore struct {
	cluster  sidecar.BlobCluster
	sign     func(payload []byte) ([]byte, error)
	settings *SidecarRevSettings
	clock    sidecar.Clock
	// the process's write budget's spend, P0 of kind revocation
	spend *apiWriteSpend
}

// A writer on cluster, signing each object's payload with sign
// (oauth.SignRevocation). A nil signer gives the expiry sweep alone.
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
		spend:    newApiWriteSpend(cluster, sidecar.WriteP0KindRevocation, settings.UnsettledWriteCount),
	}
}

// Spends every rev/ create, raise and sweep DELETE that takes effect from
// budget, as P0 of kind revocation at its row of prices, §11.13's cost rows,
// and passes every PUT and DELETE through it as P0 (see the file header). api
// and taskworker install the process's budget with their first metadata
// cluster. nil prices is the table a budget built with evsservice's default
// settings prices with; nil budget removes it.
func (self *SidecarRevStore) SetWriteBudget(budget sidecar.WriteBudget, prices *evsservice.PriceTable) {
	self.spend.setBudget(budget, prices)
}

// This process's rev/ stores, each on its own process's first metadata
// cluster: the API issuer's writer, which signs (controller's
// installApiRevocations), and taskworker's expiry sweep, which has no signer
// (taskworker's installEvsJobWriters). One of them is current, and every write
// and sweep goes through it: the issuer's while one is installed, since a
// signing store sweeps as well as writes, and the worker's only while none is,
// so in all's process, which runs api and taskworker together and installs
// both, the worker's never stands in for api's (§19.4's writers that share a
// process, owner decision 788). Each is kept in a place of its own, so the
// rule holds whichever was installed first and whichever is removed first.
// Unpublished objects stay owed in the durable queue.
var sidecarRevIssuerStore atomic.Pointer[SidecarRevStore]
var sidecarRevSweepStore atomic.Pointer[SidecarRevStore]

// The place store's role keeps: the issuer's for a store that signs, the
// worker's for a sweep.
func sidecarRevStorePlace(store *SidecarRevStore) *atomic.Pointer[SidecarRevStore] {
	if store.sign != nil {
		return &sidecarRevIssuerStore
	}
	return &sidecarRevSweepStore
}

// Installs store in its role's place until the returned function removes it,
// the removal taking out store alone, so neither of two installs in one
// process clears the other's. A sweep store installed beside a signing one
// stays out of use until the signing one is removed.
func InstallSidecarRevStore(store *SidecarRevStore) (remove func()) {
	place := sidecarRevStorePlace(store)
	place.Store(store)
	return func() {
		place.CompareAndSwap(store, nil)
	}
}

// Installs store as this process's one rev/ store, whatever was installed
// before; nil removes every store. A process with one store, or a test that
// stands for one, installs it this way.
func SetSidecarRevStore(store *SidecarRevStore) {
	sidecarRevIssuerStore.Store(nil)
	sidecarRevSweepStore.Store(nil)
	if store != nil {
		sidecarRevStorePlace(store).Store(store)
	}
}

// The current store: the issuer's, else the worker's sweep.
func currentSidecarRevStore() (*SidecarRevStore, error) {
	if store := sidecarRevIssuerStore.Load(); store != nil {
		return store, nil
	}
	if store := sidecarRevSweepStore.Load(); store != nil {
		return store, nil
	}
	return nil, errors.New("this process holds no first metadata cluster for rev/")
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
// which reads again. Each create or raise that took effect is spent once and
// counted (see the file header).
func (self *SidecarRevStore) Write(ctx context.Context, revocation *SidecarRevocation) error {
	if self.sign == nil {
		return errors.New("revocations are signed by the API issuer")
	}
	if self.settings.ObjectMaxByteCount <= 0 {
		// a write that cannot read the standing object could lower its not_before
		return errors.New("rev/ writes need a positive ObjectMaxByteCount")
	}
	key := revocation.key()
	notBefore := sidecar.RevocationNotBefore(revocation.NotBefore)
	// an object this write made keeps a not_before at or past its own until the
	// sweep, 20 minutes on, since no writer lowers one: no object, or another's
	// at or below this not_before, is a PUT of this write that never landed
	neverLanded := func(found bool, body []byte, etag string) bool {
		if !found {
			return true
		}
		held, ok := sidecarRevHeldRecord(body)
		return ok && !held.NotBefore.After(notBefore)
	}
	// this write's last PUT whose outcome was unknown, until a read settles it
	var unknownPut *apiWrite
	failed := func(err error) error {
		if unknownPut != nil {
			// no read told: spent as issued, once, for the write that settles it
			self.spend.spendUnsettled(ctx, key, unknownPut)
		}
		sidecarRevWriteCounter.WithLabelValues("failed").Inc()
		return err
	}
	for attempt := 0; attempt < self.settings.WriteAttemptCount; attempt += 1 {
		found := false
		var body []byte
		var etag string
		object, err := self.cluster.Get(ctx, key)
		switch {
		case errors.Is(err, sidecar.ErrBlobNotFound):
		case err != nil:
			return failed(err)
		default:
			body, err = io.ReadAll(io.LimitReader(object.Body, int64(self.settings.ObjectMaxByteCount)+1))
			object.Body.Close()
			if err != nil {
				return failed(err)
			}
			found, etag = true, object.Etag
		}
		writeLanded, unsettledLanded := self.spend.settleRead(ctx, key, unknownPut, found, body, etag)
		if unsettledLanded != nil {
			// an earlier write's PUT, spent as issued, landed: its outcome is known now
			sidecarRevWriteCounter.WithLabelValues(unsettledLanded.outcome).Inc()
		}
		if writeLanded {
			// this write's PUT landed and its answer was lost
			sidecarRevWriteCounter.WithLabelValues(unknownPut.outcome).Inc()
			return nil
		}
		unknownPut = nil
		if found {
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
			return failed(err)
		}
		if Testing_SidecarRevBeforePut != nil {
			Testing_SidecarRevBeforePut(key)
		}
		sidecarRevP0ByteCounter.Add(float64(len(signed)))
		putCtx := self.spend.p0Context(ctx)
		var put *apiWrite
		if !found {
			_, err = self.cluster.PutIfNoneMatch(putCtx, key, signed, nil)
			put = newApiWrite(evsservice.WriteOperationCreate, signed, "created", neverLanded)
		} else {
			// a raise writes the object again: §11.13's overwrite row
			_, err = self.cluster.PutIfMatch(putCtx, key, etag, signed, nil)
			put = newApiWrite(evsservice.WriteOperationOverwrite, signed, "raised", neverLanded)
		}
		var unknown *sidecar.BlobUnknownError
		switch {
		case err == nil:
			self.spend.spendWritten(ctx, key, put)
			sidecarRevWriteCounter.WithLabelValues(put.outcome).Inc()
			return nil
		case errors.Is(err, sidecar.ErrBlobPrecondition):
			// another writer moved the key first: this PUT wrote nothing and
			// spends nothing, and the next read decides
			continue
		case errors.As(err, &unknown):
			// this PUT may have landed: the next read decides (§11.8's unknown
			// row, D-OPS-20)
			unknownPut = put
			continue
		default:
			return failed(err)
		}
	}
	return failed(fmt.Errorf("rev/ write did not land in %d attempts", self.settings.WriteAttemptCount))
}

// Deletes every object whose last write is DeleteAfter old, reading at most
// SweepPageLimit pages. Reports how many it deleted. Each DELETE is spent at
// §11.13's delete row once the store shows it took effect, and one whose
// outcome was unknown HEADs the key again: gone, it landed, its answer lost,
// and counts; the object it meant still standing, it never landed (see the file
// header).
func (self *SidecarRevStore) Sweep(ctx context.Context) (deletedCount int, returnErr error) {
	expired := func(info *sidecar.BlobInfo) bool {
		return self.settings.DeleteAfter <= self.clock.Now().Sub(info.LastModified)
	}
	// the key's object as a HEAD sees it, found false for none
	head := func(key string) (info *sidecar.BlobInfo, found bool, err error) {
		info, err = self.cluster.Head(ctx, key)
		if errors.Is(err, sidecar.ErrBlobNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return info, true, nil
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
			info, found, err := head(listed.Key)
			if err != nil {
				return deletedCount, err
			}
			heldEtag := ""
			if found {
				heldEtag = info.Etag
			}
			// an earlier sweep's DELETE of the key, spent with its outcome
			// unknown, may settle here
			self.spend.settleRead(ctx, listed.Key, nil, found, nil, heldEtag)
			if !found || info.Etag != listed.Etag || !expired(info) {
				continue
			}
			err = self.spend.deleteObject(ctx, listed.Key, info.Etag, func() (bool, []byte, string, error) {
				info, found, err := head(listed.Key)
				if err != nil || !found {
					return false, nil, "", err
				}
				return true, nil, info.Etag, nil
			})
			if err != nil {
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
		if err != nil || store.sign == nil {
			// the issuer claims the durable task
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

// One sweep through this process's current store, the issuer's where one is
// installed and else the worker's: what taskworker's rev/ deletion runs. A
// process with no store deletes nothing, so an object outlives its 20 minutes
// rather than going early.
func SweepSidecarRevocations(ctx context.Context) (int, error) {
	store, err := currentSidecarRevStore()
	if err != nil {
		return 0, err
	}
	return store.Sweep(ctx)
}

// Only the issuer publishes signed revocations and sequenced ticks. A worker
// profile leaves these persisted target names, including versioned rows, for API.
func AuthorizationIssuerTargets() []task.Target {
	return []task.Target{
		task.NewTaskTargetWithPost(WriteSidecarRevTask, WriteSidecarRevTaskPost),
		task.NewTaskTargetWithPost(PublishConfigLogTickTask, AppendConfigLogTickTaskPost),
		task.NewTaskTargetWithPost(PublishConfigLogActivationTask, AppendConfigLogActivationTaskPost),
	}
}
