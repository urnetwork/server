package stats

// ship finalized local stats segments to blob storage.
//
// The Uploader watches the on-disk stats tree for finalized `.pb.zst` segments,
// ships each to the configured blob store (server.BlobStore, S3/MinIO-backed
// today), and deletes the local copy on success. Retention is the store's job:
// an ILM lifecycle rule (set by ops, see xops) expires objects after the
// rolling window, so the uploader only ever writes. When the store is
// unreachable the segments accumulate locally up to a disk cap, past which the
// oldest are dropped (and counted) rather than filling the disk — the same
// drop-don't-block discipline as the writer.
//
// Object key layout (date-partitioned for the export tooling):
//
//	<prefix>/<env>/<stream>/<yyyy-mm-dd>/<instance-label>/<segment>
//
// derived from the local path <stats root>/<env>/<label>/<stream>/<segment>.
//
// The uploader is used by the long-lived api service (ships its own samples);
// bringyourctl's export command reads the same objects back (see export.go).
// All object-storage specifics live in server/blob.go.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// UploaderSettings tunes the shipping loop.
type UploaderSettings struct {
	Interval time.Duration
	// local segments cap (bytes) held when the store is unreachable; the oldest
	// finalized segments over this are dropped
	LocalCapBytes int64
}

func DefaultUploaderSettings() *UploaderSettings {
	return &UploaderSettings{
		Interval:      30 * time.Second,
		LocalCapBytes: 8 * 1024 * 1024 * 1024, // 8 GiB
	}
}

// Uploader ships finalized segments under statsRoot to the blob store.
type Uploader struct {
	ctx       context.Context
	statsRoot string
	env       string
	store     server.BlobStore
	settings  *UploaderSettings

	uploaded atomic.Uint64
	dropped  atomic.Uint64
	failed   atomic.Uint64
}

// maxSweepFailures bounds failed upload attempts within one sweep: a fully
// unreachable store bails out after this many attempts (retrying next tick)
// instead of stretching one sweep across every segment's timeout, which would
// also delay the disk-cap enforcement below.
const maxSweepFailures = 16

// StartUpload begins shipping segments for the process's stats, if a blob store
// is configured (vault minio.yml). Returns nil (and does nothing) when disabled.
// Call after Enable.
func (self *Stats) StartUpload(settings *UploaderSettings) (*Uploader, error) {
	if !self.enabled {
		return nil, nil
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		glog.Infof("[stats]upload disabled: no blob store (minio.yml)\n")
		return nil, nil
	}
	env, _ := server.Env()
	// the stats root is the parent of the instance dir (…/stats/<env>/<label>)
	statsRoot := filepath.Dir(filepath.Dir(self.root))
	return startUploader(self.ctx, statsRoot, env, store, settings)
}

func startUploader(ctx context.Context, statsRoot string, env string, store server.BlobStore, settings *UploaderSettings) (*Uploader, error) {
	if settings == nil {
		settings = DefaultUploaderSettings()
	}
	self := &Uploader{
		ctx:       ctx,
		statsRoot: statsRoot,
		env:       env,
		store:     store,
		settings:  settings,
	}
	go server.HandleError(self.run)
	glog.Infof("[stats]upload -> %s/%s\n", store.Authority(), store.Bucket())
	// note: per-stream retention (lifecycle rules) is applied out of band by
	// the taskworker via stats.ApplyStreamRetention — not here, since the api's
	// upload path is write-only and may lack lifecycle permissions.
	return self, nil
}

func (self *Uploader) run() {
	ticker := time.NewTicker(self.settings.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-ticker.C:
			self.sweep()
		}
	}
}

// sweep uploads every finalized segment under the stats root, deleting each on
// success. A failing segment is logged, counted, and left in place for the
// next sweep while the sweep CONTINUES with the remaining segments — one
// poison segment must not starve everything behind it (it used to block the
// whole sweep every tick until the disk cap eventually dropped it). A
// store-wide outage is bounded by maxSweepFailures. The local disk cap is
// enforced whenever anything failed.
func (self *Uploader) sweep() {
	segments := self.finalizedSegments()
	uploadedAny := false
	failedCount := 0
	for _, segment := range segments {
		if self.ctx.Err() != nil {
			break
		}
		if failedCount >= maxSweepFailures {
			// the store looks unreachable, not one segment poisoned; retry
			// the rest next tick
			break
		}
		if err := self.upload(segment); err != nil {
			glog.Infof("[stats]upload %s err=%s\n", segment, err)
			self.failed.Add(1)
			failedCount += 1
			continue
		}
		os.Remove(segment)
		self.uploaded.Add(1)
		uploadedAny = true
	}
	if failedCount > 0 {
		self.enforceCap()
	}
	if uploadedAny || failedCount > 0 {
		glog.Infof("[stats]uploaded=%d failed=%d dropped=%d\n",
			self.uploaded.Load(), self.failed.Load(), self.dropped.Load())
	}
}

func (self *Uploader) upload(segment string) error {
	key, err := self.objectKey(segment)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(self.ctx, 60*time.Second)
	defer cancel()
	return self.store.Put(ctx, key, segment, "application/zstd")
}

// objectKey maps a local segment path to its date-partitioned object key.
// local:  <statsRoot>/<env>/<label>/<stream>/<seg>
// object: <prefix>/<env>/<stream>/<yyyy-mm-dd>/<label>/<seg>
func (self *Uploader) objectKey(segment string) (string, error) {
	rel, err := filepath.Rel(self.statsRoot, segment)
	if err != nil {
		return "", err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 4 {
		return "", fmt.Errorf("unexpected segment path %q", rel)
	}
	env, label, stream, seg := parts[0], parts[1], parts[2], parts[3]
	day := segmentDay(seg)
	return strings.Join([]string{self.store.Prefix(), env, stream, day, label, seg}, "/"), nil
}

// segmentDay parses the yyyy-mm-dd from a segment name (<unixmilli>-<seq>.pb.zst).
func segmentDay(seg string) string {
	dash := strings.IndexByte(seg, '-')
	if dash <= 0 {
		return "unknown"
	}
	var millis int64
	if _, err := fmt.Sscanf(seg[:dash], "%d", &millis); err != nil {
		return "unknown"
	}
	return time.UnixMilli(millis).UTC().Format("2006-01-02")
}

func (self *Uploader) finalizedSegments() []string {
	segments := []string{}
	filepath.Walk(self.statsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, segmentExt) { // .pb.zst, never .partial
			segments = append(segments, path)
		}
		return nil
	})
	return segments
}

// enforceCap drops the oldest finalized segments while the local total exceeds
// the cap, so an unreachable store cannot fill the disk.
func (self *Uploader) enforceCap() {
	type seg struct {
		path  string
		size  int64
		mtime time.Time
	}
	segs := []seg{}
	var total int64
	for _, path := range self.finalizedSegments() {
		if info, err := os.Stat(path); err == nil {
			segs = append(segs, seg{path: path, size: info.Size(), mtime: info.ModTime()})
			total += info.Size()
		}
	}
	if total <= self.settings.LocalCapBytes {
		return
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].mtime.Before(segs[j].mtime) })
	for _, s := range segs {
		if total <= self.settings.LocalCapBytes {
			break
		}
		os.Remove(s.path)
		total -= s.size
		self.dropped.Add(1)
	}
	glog.Infof("[stats]local cap enforced: dropped oldest, %d total dropped\n", self.dropped.Load())
}
