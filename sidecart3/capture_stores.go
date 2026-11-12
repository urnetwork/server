package sidecart3

// §22.7's CC3 on T3 (TEST-M2-09; R-TEST-3, I-1, I-2, I-18): what T3's real stores hold, added to a Net's capture as
// sources its scan reads at each Scan, so a canary or a key in any stored byte is found in every form the scan
// knows, never sampled. Each item streams: the scan pulls it a part at a time (capturePartReader), and a part is read
// from its store only when the scan reaches it, so no object, Redis key or table is held whole:
//
//   - every object in every metadata cluster and the segment cluster: its key, which is data too, its metadata and
//     its body, read through the Net's host under InspectContext, so the capture takes no plan and leaves no
//     history (FI-30's recorder);
//   - every Redis key on every master of the test's database (the ForEach over masters a 3-master cluster needs, one
//     node until QD-028's change): its name, DUMP's payload, and a read by the key's type a range at a time (GETRANGE,
//     HSCAN, LRANGE, SSCAN, ZRANGE with scores, XRANGE), since DUMP's encoding of a listpack or a compressed list can
//     hide a canary the plain values show. DUMP's payload is one reply, the one part a key holds whole;
//   - every row of every sidecar Postgres table local/sidecar-columns.yml names, each row as Postgres's JSON text of
//     it, bytea as its hex, a page at a time in ctid order, each page in a read-only transaction of its own: a table a
//     group this binary does not link has none, and is listed as not read.
//
// What stays whole until the scan reads it is the list of names: each cluster's keys a page at a time as it lists,
// and every Redis key's name, since SCAN may answer a name twice.
//
// PrivacyClassPostgres registers here, beside its capture, and capture_stores_leak.go holds its leak under
// sidecar_leaktest. A store that cannot be read, listed or scanned yields an item that will not open, and a part that
// fails ends its item's stream with the failure, so the scan reports each as a finding no run passes on; the report
// lists them too: what was not read was not scanned.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// CC3's Postgres class, T3's alone, which T2's harness leaves unregistered (sidecartest's privacy_sources.go).
var _ = sidecartest.RegisterPrivacyClass(sidecartest.PrivacyClassPostgres)

// What CC3 reads on T3.
type CaptureStoresSettings struct {
	// local/sidecar-columns.yml, from this package's directory: the sidecar tables to read.
	SidecarColumnsPath string
	// Tables read beside the file's, for a test's own.
	ExtraPostgresTables []string
	// Keys one LIST of a cluster asks for: 1,000. Positive.
	ListPageCount int
	// Keys one SCAN asks for: 1,000. Positive.
	RedisScanCount int64
	// Rows one Postgres page reads: 1,000. Positive.
	PostgresPageRowCount int
	// Elements one range read of a hash, a list, a set, a sorted set or a stream asks for: 1,000. Positive.
	RedisRangeCount int64
	// Bytes one GETRANGE of a string reads: 1 MiB, the scan's own chunk. Positive.
	RedisStringChunkByteCount int64
}

func DefaultCaptureStoresSettings() *CaptureStoresSettings {
	return &CaptureStoresSettings{
		SidecarColumnsPath:        "../local/sidecar-columns.yml",
		ListPageCount:             1000,
		RedisScanCount:            1000,
		PostgresPageRowCount:      1000,
		RedisRangeCount:           1000,
		RedisStringChunkByteCount: 1024 * 1024,
	}
}

// A Postgres table name as the column map writes it.
var captureTableNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// The sidecar tables local/sidecar-columns.yml names, sorted.
func sidecarTables(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	columns := &struct {
		Tables map[string]yaml.Node `yaml:"tables"`
	}{}
	if err := yaml.Unmarshal(data, columns); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	tables := []string{}
	for table := range columns.Tables {
		if !captureTableNamePattern.MatchString(table) {
			return nil, fmt.Errorf("%s names table %q", path, table)
		}
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables, nil
}

// What CaptureStores read at the last scan: the stores' counts, and what it could not read.
type CaptureStoresReport struct {
	stateLock sync.Mutex
	// objects, Redis keys and Postgres rows read
	objectCount int
	redisCount  int
	rowCount    int
	// tables no group this binary links has made
	tablesNotRead []string
	// what a store answered when asked
	errs []error
}

func (self *CaptureStoresReport) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.objectCount, self.redisCount, self.rowCount = 0, 0, 0
	self.tablesNotRead = nil
	self.errs = nil
}

func (self *CaptureStoresReport) failed(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.errs = append(self.errs, err)
}

// How many objects, Redis keys and Postgres rows the last scan read.
func (self *CaptureStoresReport) Counts() (objects int, redisKeys int, rows int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.objectCount, self.redisCount, self.rowCount
}

// The sidecar tables the last scan found no table for.
func (self *CaptureStoresReport) TablesNotRead() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.tablesNotRead)
}

// What a store failed to answer at the last scan: a store not read is a scan that passed on nothing.
func (self *CaptureStoresReport) Errors() []error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.errs)
}

// Adds T3's stores to net's capture (CC3): every object, every Redis key and every sidecar table row, read again at
// each Scan. settings nil takes the defaults. The report says what each scan read and what it could not.
func CaptureStores(net *sidecartest.Net, settings *CaptureStoresSettings) (*CaptureStoresReport, error) {
	if settings == nil {
		settings = DefaultCaptureStoresSettings()
	}
	tables, err := sidecarTables(settings.SidecarColumnsPath)
	if err != nil {
		return nil, err
	}
	tables = append(tables, settings.ExtraPostgresTables...)
	report := &CaptureStoresReport{}
	capture := net.Capture()
	capture.AddSource(func() []*sidecartest.PrivacyItem {
		report.reset()
		ctx := sidecartest.InspectContext(context.WithoutCancel(net.Context()))
		items := []*sidecartest.PrivacyItem{}
		items = append(items, captureObjects(ctx, net.Host().Blobs, settings, report)...)
		items = append(items, captureRedis(ctx, settings, report)...)
		items = append(items, capturePostgres(ctx, tables, settings, report)...)
		return items
	})
	return report, nil
}

// Every object of every cluster, each an item that streams its key, metadata and body.
func captureObjects(ctx context.Context, blobs sidecar.BlobStore, settings *CaptureStoresSettings, report *CaptureStoresReport) []*sidecartest.PrivacyItem {
	items := []*sidecartest.PrivacyItem{}
	clusters := []sidecar.BlobCluster{}
	for _, clusterId := range blobs.MetadataClusterIds() {
		cluster, err := blobs.Cluster(clusterId)
		if err != nil {
			items = append(items, captureUnread(sidecartest.PrivacyClassObject, "cluster "+clusterId, err, report))
			continue
		}
		clusters = append(clusters, cluster)
	}
	if segmentCluster, err := blobs.SegmentCluster(); err == nil {
		clusters = append(clusters, segmentCluster)
	} else {
		items = append(items, captureUnread(sidecartest.PrivacyClassObject, "the segment cluster", err, report))
	}
	for _, cluster := range clusters {
		startAfter := ""
		for {
			objects, more, err := cluster.List(ctx, "", startAfter, settings.ListPageCount)
			if err != nil {
				items = append(items, captureUnread(sidecartest.PrivacyClassObject, "listing cluster "+cluster.ClusterId(), err, report))
				break
			}
			for _, object := range objects {
				startAfter = object.Key
				items = append(items, captureObjectItem(ctx, cluster, object.Key, report))
			}
			report.stateLock.Lock()
			report.objectCount += len(objects)
			report.stateLock.Unlock()
			if !more {
				break
			}
		}
	}
	return items
}

func captureObjectItem(ctx context.Context, cluster sidecar.BlobCluster, key string, report *CaptureStoresReport) *sidecartest.PrivacyItem {
	return &sidecartest.PrivacyItem{
		Class: sidecartest.PrivacyClassObject,
		Name:  "object " + cluster.ClusterId() + " " + key,
		Open: func() (io.ReadCloser, error) {
			object, err := cluster.Get(ctx, key)
			if err != nil {
				report.failed(fmt.Errorf("reading %s on %s: %w", key, cluster.ClusterId(), err))
				// the key alone, which is data too
				return io.NopCloser(strings.NewReader(key)), nil
			}
			metadataKeys := []string{}
			for metadataKey := range object.Metadata {
				metadataKeys = append(metadataKeys, metadataKey)
			}
			sort.Strings(metadataKeys)
			var head bytes.Buffer
			head.WriteString(key + "\x00")
			for _, metadataKey := range metadataKeys {
				head.WriteString(metadataKey + "=" + object.Metadata[metadataKey] + "\x00")
			}
			return &captureObjectBody{Reader: io.MultiReader(&head, object.Body), body: object.Body}, nil
		},
	}
}

// An object's key, metadata and body as one stream, which closes the body.
type captureObjectBody struct {
	io.Reader
	body io.Closer
}

func (self *captureObjectBody) Close() error {
	return self.body.Close()
}

// An item whose store could not be read, which will not open, so the scan reports it as a finding: what was not read
// was not scanned. The report lists it too.
func captureUnread(class sidecartest.PrivacyClass, name string, err error, report *CaptureStoresReport) *sidecartest.PrivacyItem {
	err = fmt.Errorf("%s: %w", name, err)
	report.failed(err)
	return &sidecartest.PrivacyItem{Class: class, Name: name, Open: func() (io.ReadCloser, error) {
		return nil, err
	}}
}

// A stream of the parts a producer yields, each made only when the scan has read the one before, so a store's read is
// held a part at a time, never whole. A part's error ends the stream with it, which the scan reports. Not safe for
// concurrent use: the scan reads it from one goroutine.
type capturePartReader struct {
	next func() ([]byte, error, bool)
	stop func()
	part []byte
	err  error
}

func newCapturePartReader(parts iter.Seq2[[]byte, error]) *capturePartReader {
	next, stop := iter.Pull2(parts)
	return &capturePartReader{next: next, stop: stop}
}

func (self *capturePartReader) Read(p []byte) (int, error) {
	for len(self.part) == 0 {
		if self.err != nil {
			return 0, self.err
		}
		part, err, ok := self.next()
		switch {
		case !ok:
			self.err = io.EOF
		case err != nil:
			self.err = err
		default:
			self.part = part
		}
	}
	n := copy(p, self.part)
	self.part = self.part[n:]
	return n, nil
}

// Ends the producer where it stands.
func (self *capturePartReader) Close() error {
	self.stop()
	return nil
}

// Every Redis key of the test's database on every master, each an item that streams its name, DUMP's payload and its
// values a range at a time.
func captureRedis(ctx context.Context, settings *CaptureStoresSettings, report *CaptureStoresReport) (items []*sidecartest.PrivacyItem) {
	defer func() {
		// server.Redis raises a pool's failure as a panic
		if recovered := recover(); recovered != nil {
			items = append(items, captureUnread(sidecartest.PrivacyClassRedis, "the capture's Redis", fmt.Errorf("%v", recovered), report))
		}
	}()
	var client server.RedisClient
	server.Redis(ctx, func(redisClient server.RedisClient) {
		client = redisClient
	})
	keys := []string{}
	var keysLock sync.Mutex
	scanNode := func(ctx context.Context, node redis.Cmdable) error {
		cursor := uint64(0)
		for {
			found, next, err := node.Scan(ctx, cursor, "*", settings.RedisScanCount).Result()
			if err != nil {
				return err
			}
			keysLock.Lock()
			keys = append(keys, found...)
			keysLock.Unlock()
			if next == 0 {
				return nil
			}
			cursor = next
		}
	}
	var err error
	if clusterClient, ok := client.(*redis.ClusterClient); ok {
		err = clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			return scanNode(ctx, node)
		})
	} else {
		err = scanNode(ctx, client)
	}
	if err != nil {
		items = append(items, captureUnread(sidecartest.PrivacyClassRedis, "SCAN", err, report))
	}
	sort.Strings(keys)
	keys = slices.Compact(keys)
	for _, key := range keys {
		items = append(items, &sidecartest.PrivacyItem{
			Class: sidecartest.PrivacyClassRedis,
			Name:  "redis " + key,
			Open: func() (io.ReadCloser, error) {
				return newCapturePartReader(captureRedisKeyParts(ctx, client, key, settings, report)), nil
			},
		})
	}
	report.stateLock.Lock()
	report.redisCount += len(keys)
	report.stateLock.Unlock()
	return items
}

// key's name, DUMP's payload, and its values read by its type a range at a time, each value apart from the next. A key
// gone since the SCAN is its name alone.
func captureRedisKeyParts(ctx context.Context, client server.RedisClient, key string, settings *CaptureStoresSettings, report *CaptureStoresReport) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		values := func(values ...string) []byte {
			var part bytes.Buffer
			for _, value := range values {
				part.WriteString(value + "\x00")
			}
			return part.Bytes()
		}
		failed := func(err error) {
			err = fmt.Errorf("reading Redis key %q: %w", key, err)
			report.failed(err)
			yield(nil, err)
		}
		if !yield(values(key), nil) {
			return
		}
		dump, err := client.Dump(ctx, key).Result()
		if err == redis.Nil {
			return
		}
		if err != nil {
			failed(err)
			return
		}
		if !yield(values(dump), nil) {
			return
		}
		keyType, err := client.Type(ctx, key).Result()
		if err != nil {
			failed(err)
			return
		}
		switch keyType {
		case "string":
			for start := int64(0); ; start += settings.RedisStringChunkByteCount {
				chunk, err := client.GetRange(ctx, key, start, start+settings.RedisStringChunkByteCount-1).Result()
				if err != nil {
					failed(err)
					return
				}
				if !yield([]byte(chunk), nil) {
					return
				}
				if int64(len(chunk)) < settings.RedisStringChunkByteCount {
					yield(values(""), nil)
					return
				}
			}
		case "hash":
			cursor := uint64(0)
			for {
				fieldValues, next, err := client.HScan(ctx, key, cursor, "*", settings.RedisRangeCount).Result()
				if err != nil {
					failed(err)
					return
				}
				if !yield(values(fieldValues...), nil) || next == 0 {
					return
				}
				cursor = next
			}
		case "list":
			for start := int64(0); ; start += settings.RedisRangeCount {
				elements, err := client.LRange(ctx, key, start, start+settings.RedisRangeCount-1).Result()
				if err != nil {
					failed(err)
					return
				}
				if !yield(values(elements...), nil) || int64(len(elements)) < settings.RedisRangeCount {
					return
				}
			}
		case "set":
			cursor := uint64(0)
			for {
				members, next, err := client.SScan(ctx, key, cursor, "*", settings.RedisRangeCount).Result()
				if err != nil {
					failed(err)
					return
				}
				if !yield(values(members...), nil) || next == 0 {
					return
				}
				cursor = next
			}
		case "zset":
			for start := int64(0); ; start += settings.RedisRangeCount {
				members, err := client.ZRangeWithScores(ctx, key, start, start+settings.RedisRangeCount-1).Result()
				if err != nil {
					failed(err)
					return
				}
				memberScores := []string{}
				for _, member := range members {
					memberScores = append(memberScores, fmt.Sprint(member.Member), fmt.Sprint(member.Score))
				}
				if !yield(values(memberScores...), nil) || int64(len(members)) < settings.RedisRangeCount {
					return
				}
			}
		case "stream":
			after := "-"
			for {
				messages, err := client.XRangeN(ctx, key, after, "+", settings.RedisRangeCount).Result()
				if err != nil {
					failed(err)
					return
				}
				entries := []string{}
				for _, message := range messages {
					fields := []string{}
					for field := range message.Values {
						fields = append(fields, field)
					}
					sort.Strings(fields)
					entries = append(entries, message.ID)
					for _, field := range fields {
						entries = append(entries, field, fmt.Sprint(message.Values[field]))
					}
					after = "(" + message.ID
				}
				if !yield(values(entries...), nil) || int64(len(messages)) < settings.RedisRangeCount {
					return
				}
			}
		}
	}
}

// Every row of each table, each table an item of Postgres's JSON text of its rows, read a page at a time as the scan
// reaches it.
func capturePostgres(ctx context.Context, tables []string, settings *CaptureStoresSettings, report *CaptureStoresReport) []*sidecartest.PrivacyItem {
	reader := &postgresReader{}
	items := []*sidecartest.PrivacyItem{}
	present := []string{}
	for _, table := range tables {
		rows, err := reader.Rows(ctx, "SELECT to_regclass($1) IS NOT NULL", table)
		if err != nil {
			items = append(items, captureUnread(sidecartest.PrivacyClassPostgres, "finding table "+table, err, report))
			continue
		}
		if len(rows) == 1 && rows[0][0] == true {
			present = append(present, table)
		} else {
			report.stateLock.Lock()
			report.tablesNotRead = append(report.tablesNotRead, table)
			report.stateLock.Unlock()
		}
	}
	for _, table := range present {
		items = append(items, &sidecartest.PrivacyItem{
			Class: sidecartest.PrivacyClassPostgres,
			Name:  "postgres " + table,
			Open: func() (io.ReadCloser, error) {
				return newCapturePartReader(capturePostgresTableParts(ctx, reader, table, settings, report)), nil
			},
		})
	}
	return items
}

// table's rows a page at a time, after the last page's final ctid, each row its JSON text on a line. Each page is a
// read-only transaction of its own, so a row moved by a write between two pages can be read twice or not at all: the
// capture reads at a flow's end, when nothing writes.
func capturePostgresTableParts(ctx context.Context, reader *postgresReader, table string, settings *CaptureStoresSettings, report *CaptureStoresReport) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		identifier := pgx.Identifier{table}.Sanitize()
		query := fmt.Sprintf("SELECT ctid::text, row_to_json(t)::text FROM %s t WHERE ctid > $1::tid ORDER BY ctid LIMIT %d", identifier, settings.PostgresPageRowCount)
		after := "(0,0)"
		for {
			rows, err := reader.Rows(ctx, query, after)
			if err != nil {
				err = fmt.Errorf("reading table %s: %w", table, err)
				report.failed(err)
				yield(nil, err)
				return
			}
			var page bytes.Buffer
			for _, row := range rows {
				if ctid, ok := row[0].(string); ok {
					after = ctid
				}
				if text, ok := row[1].(string); ok {
					page.WriteString(text + "\n")
				} else {
					encoded, _ := json.Marshal(row[1])
					page.Write(append(encoded, '\n'))
				}
			}
			report.stateLock.Lock()
			report.rowCount += len(rows)
			report.stateLock.Unlock()
			if !yield(page.Bytes(), nil) || len(rows) < settings.PostgresPageRowCount {
				return
			}
		}
	}
}
