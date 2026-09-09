// Ready immutable registrations share finite local quota windows only. Sql,
// source authority, signatures and per-client deadlines remain independently owned.
package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

const stClientKeyRegistrationPublicationBatch = 64

// One admitted member owns these bounded bytes until its synchronous result.
// Only the worker writes err, before closing done after the final quota census.
type stClientKeyRegistrationPublication struct {
	ctx         context.Context
	store       server.BlobStore
	encoded     []byte
	contentHash string
	done        chan struct{}
	err         error
}

// Unknown and remote backends retain their independent publication path. Only
// an existing authority reservation may queue local work, once, after its Sql
// result exists. Waiting for the worker also joins this member's actual writes.
func (self *stClientKeyRegistrationReservation) publish(ctx context.Context, store server.BlobStore, encoded []byte, contentHash string) error {
	if self == nil || self.cohort == nil || self.cohort.parent == nil || ctx == nil || store == nil || len(encoded) == 0 || len(encoded) > startifact.MaxClientKeyEvidenceBytes || len(contentHash) > 128 {
		return errors.New("client-key registration publication owner is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// This cannot extend the original operation deadline. It also bounds a
	// private direct caller that did not already install that operation owner.
	ctx, cancel := context.WithTimeout(ctx, stCallTimeout)
	defer cancel()
	parent := self.cohort.parent
	_, local := store.(server.LocalBlobBatchSource)
	request := &stClientKeyRegistrationPublication{ctx: ctx, store: store, encoded: bytes.Clone(encoded), contentHash: contentHash, done: make(chan struct{})}
	start := false
	var before func(context.Context)
	var admitted func()
	err := func() error {
		parent.stateLock.Lock()
		defer parent.stateLock.Unlock()
		if self.released || self.waiting || self.publicationClaimed || !self.cohort.finished || self.cohort.err != nil {
			return errors.New("client-key registration publication lacks its completed live reservation")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if parent.slots <= 0 || len(parent.publications) >= maxStClientKeyRegistrationSlots {
			return errors.New("client-key registration publication finite capacity is unavailable")
		}
		self.publicationClaimed = true
		if local {
			parent.publications = append(parent.publications, request)
			if !parent.publicationRunning {
				parent.publicationRunning = true
				start, before = true, parent.beforePublicationForTest
			}
			admitted = parent.afterPublicationAdmissionForTest
		}
		return nil
	}()
	if err != nil {
		return err
	}
	if !local {
		_, err := startifact.PublishClientKeyEvidence(ctx, store, request.encoded, contentHash)
		return errors.Join(err, ctx.Err())
	}
	if start {
		go parent.runPublications(ctx, before)
	}
	if admitted != nil {
		admitted()
	}
	<-request.done
	return errors.Join(request.err, ctx.Err())
}

// Drain only already-ready work, never wait for another Sql result or future
// registration while holding a filesystem lock. One worker ends at quiescence;
// the existing 1,024 complete-member slots also bound its queued byte owners.
func (self *stClientKeyRegistrationCohorts) runPublications(ctx context.Context, before func(context.Context)) {
	var active []*stClientKeyRegistrationPublication
	finished := false
	defer func() {
		if finished {
			return
		}
		cause := errors.New("client-key registration publication worker exited without completion")
		if recovered := recover(); recovered != nil {
			cause = fmt.Errorf("client-key registration publication worker panicked: %v", recovered)
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			active = append(active, self.publications...)
			self.publications = nil
			self.publicationRunning = false
		}()
		for _, request := range active {
			request.err = errors.Join(request.err, cause, request.ctx.Err())
			close(request.done)
		}
	}()
	if before != nil {
		before(ctx)
	}
	for {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			count := min(len(self.publications), stClientKeyRegistrationPublicationBatch)
			if count == 0 {
				self.publications = nil
				self.publicationRunning = false
				finished = true
				return
			}
			active = append([]*stClientKeyRegistrationPublication(nil), self.publications[:count]...)
			clear(self.publications[:count])
			self.publications = self.publications[count:]
		}()
		if finished {
			return
		}
		publishStClientKeyRegistrationWindow(active, self.afterPublicationWindowForTest, self.afterPublicationForTest)
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if len(self.publications) == 0 {
				self.publications = nil
				self.publicationRunning = false
				finished = true
			}
		}()
		for _, request := range active {
			close(request.done)
		}
		active = nil
		if finished {
			return
		}
	}
}

// Each ready window binds its exact stores and retains its final Close result
// before releasing any member. A canceled member cannot cancel a live sibling;
// canceling all members ends root acquisition and joins every callback.
func publishStClientKeyRegistrationWindow(requests []*stClientKeyRegistrationPublication, afterWindow func(*server.LocalBlobWriteBatch, int), afterPublication func()) {
	var batch *server.LocalBlobWriteBatch
	var groupErr error
	var cleanup func()
	defer func() {
		if recovered := recover(); recovered != nil {
			groupErr = fmt.Errorf("client-key registration publication panicked: %v", recovered)
		}
		groupErr = errors.Join(groupErr, batch.Close())
		if cleanup != nil {
			cleanup()
		}
		for _, request := range requests {
			request.err = errors.Join(request.err, groupErr, request.ctx.Err())
		}
	}()
	var ready []*stClientKeyRegistrationPublication
	var stores []server.BlobStore
	for _, request := range requests {
		if err := request.ctx.Err(); err != nil {
			request.err = err
			continue
		}
		ready = append(ready, request)
		stores = append(stores, request.store)
	}
	if len(ready) == 0 {
		return
	}
	// This context owns only shared filesystem coordination. Actual writes
	// bind their original context below, including its earlier deadline.
	batchCtx, cancel := context.WithTimeout(context.Background(), stCallTimeout)
	var remaining atomic.Int32
	remaining.Store(int32(len(ready)))
	stops := make([]func() bool, 0, len(ready))
	joined := make([]chan struct{}, 0, len(ready))
	for _, request := range ready {
		done := make(chan struct{})
		joined = append(joined, done)
		stops = append(stops, context.AfterFunc(request.ctx, func() {
			defer close(done)
			if remaining.Add(-1) == 0 {
				cancel()
			}
		}))
	}
	cleanup = func() {
		for index, stop := range stops {
			if !stop() {
				<-joined[index]
			}
		}
		cancel()
	}
	batch, groupErr = server.BeginLocalBlobWriteBatch(batchCtx, stores, 2*len(ready))
	if groupErr != nil {
		return
	}
	if batch == nil {
		groupErr = errors.New("client-key registration local publication lost its declared quota source")
		return
	}
	if afterWindow != nil {
		afterWindow(batch, len(ready))
	}
	for _, request := range ready {
		publicationCtx, err := batch.ContextFor(request.ctx)
		if err == nil {
			_, err = startifact.PublishClientKeyEvidence(publicationCtx, request.store, request.encoded, request.contentHash)
		}
		request.err = err
		if err == nil && afterPublication != nil {
			afterPublication()
		}
	}
}
