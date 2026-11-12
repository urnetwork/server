package sidecart3

// §22.7's CC3 on T3 (TEST-M2-09; R-TEST-3, I-1, I-2, I-18): what T3's real stores hold, added to a Net's capture as
// sources its scan reads at each Scan, so a canary or a key in any stored byte is found in every form the scan
// knows, never sampled (an object streams to the scan; a Redis key's reads and a Postgres table's pages are held
// whole until the scan reads them, which this draft leaves to the review phase):
//
//   - every object in every metadata cluster and the segment cluster: its key, which is data too, its metadata and
//     its body, read through the Net's host under InspectContext, so the capture takes no plan and leaves no
//     history (FI-30's recorder);
//   - every Redis key on every master of the test's database (the ForEach over masters a 3-master cluster needs, one
//     node until QD-028's change): its name, DUMP's payload, and a read by the key's type (GET, HGETALL, LRANGE,
//     SMEMBERS, ZRANGE with scores, XRANGE), since DUMP's encoding of a listpack or a compressed list can hide a
//     canary the plain values show;
//   - every row of every sidecar Postgres table local/sidecar-columns.yml names, each row as Postgres's JSON text of
//     it, bytea as its hex: a table a group this binary does not link has none, and is listed as not read.
//
// PrivacyClassPostgres registers here, beside its capture, and capture_stores_leak.go holds its leak under
// sidecar_leaktest. A source that cannot read its store yields an item saying so, which the scan cannot pass for a
// canary, and CaptureStores' report lists it: what was not read was not scanned.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	// Elements one range read of a list, a set, a sorted set or a stream asks for: 1,000. Positive.
	RedisRangeCount int64
}

func DefaultCaptureStoresSettings() *CaptureStoresSettings {
	return &CaptureStoresSettings{
		SidecarColumnsPath:   "../local/sidecar-columns.yml",
		ListPageCount:        1000,
		RedisScanCount:       1000,
		PostgresPageRowCount: 1000,
		RedisRangeCount:      1000,
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
	clusters := []sidecar.BlobCluster{}
	for _, clusterId := range blobs.MetadataClusterIds() {
		cluster, err := blobs.Cluster(clusterId)
		if err != nil {
			report.failed(err)
			continue
		}
		clusters = append(clusters, cluster)
	}
	if segmentCluster, err := blobs.SegmentCluster(); err == nil {
		clusters = append(clusters, segmentCluster)
	} else {
		report.failed(err)
	}
	items := []*sidecartest.PrivacyItem{}
	for _, cluster := range clusters {
		startAfter := ""
		for {
			objects, more, err := cluster.List(ctx, "", startAfter, settings.ListPageCount)
			if err != nil {
				report.failed(fmt.Errorf("listing cluster %s: %w", cluster.ClusterId(), err))
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

// Every Redis key of the test's database on every master, each an item of its name, DUMP's payload and its values.
func captureRedis(ctx context.Context, settings *CaptureStoresSettings, report *CaptureStoresReport) (items []*sidecartest.PrivacyItem) {
	defer func() {
		// server.Redis raises a pool's failure as a panic
		if recovered := recover(); recovered != nil {
			report.failed(fmt.Errorf("the capture's Redis: %v", recovered))
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
		report.failed(fmt.Errorf("SCAN: %w", err))
	}
	sort.Strings(keys)
	keys = slices.Compact(keys)
	for _, key := range keys {
		body, err := captureRedisKey(ctx, client, key, settings)
		if err != nil {
			report.failed(fmt.Errorf("reading Redis key %q: %w", key, err))
		}
		items = append(items, &sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassRedis, Name: "redis " + key, Body: body})
	}
	report.stateLock.Lock()
	report.redisCount += len(keys)
	report.stateLock.Unlock()
	return items
}

// key's name, DUMP's payload, and its values read by its type, each part apart from the next.
func captureRedisKey(ctx context.Context, client server.RedisClient, key string, settings *CaptureStoresSettings) ([]byte, error) {
	var body bytes.Buffer
	body.WriteString(key + "\x00")
	dump, err := client.Dump(ctx, key).Result()
	if err == redis.Nil {
		// gone since the SCAN
		return body.Bytes(), nil
	}
	if err != nil {
		return body.Bytes(), err
	}
	body.WriteString(dump + "\x00")
	keyType, err := client.Type(ctx, key).Result()
	if err != nil {
		return body.Bytes(), err
	}
	write := func(values ...string) {
		for _, value := range values {
			body.WriteString(value + "\x00")
		}
	}
	switch keyType {
	case "string":
		value, err := client.Get(ctx, key).Result()
		if err != nil && err != redis.Nil {
			return body.Bytes(), err
		}
		write(value)
	case "hash":
		fieldValues, err := client.HGetAll(ctx, key).Result()
		if err != nil {
			return body.Bytes(), err
		}
		fields := []string{}
		for field := range fieldValues {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			write(field, fieldValues[field])
		}
	case "list":
		for start := int64(0); ; start += settings.RedisRangeCount {
			values, err := client.LRange(ctx, key, start, start+settings.RedisRangeCount-1).Result()
			if err != nil {
				return body.Bytes(), err
			}
			write(values...)
			if int64(len(values)) < settings.RedisRangeCount {
				break
			}
		}
	case "set":
		cursor := uint64(0)
		for {
			members, next, err := client.SScan(ctx, key, cursor, "*", settings.RedisRangeCount).Result()
			if err != nil {
				return body.Bytes(), err
			}
			write(members...)
			if next == 0 {
				break
			}
			cursor = next
		}
	case "zset":
		for start := int64(0); ; start += settings.RedisRangeCount {
			members, err := client.ZRangeWithScores(ctx, key, start, start+settings.RedisRangeCount-1).Result()
			if err != nil {
				return body.Bytes(), err
			}
			for _, member := range members {
				write(fmt.Sprint(member.Member), fmt.Sprint(member.Score))
			}
			if int64(len(members)) < settings.RedisRangeCount {
				break
			}
		}
	case "stream":
		after := "-"
		for {
			messages, err := client.XRangeN(ctx, key, after, "+", settings.RedisRangeCount).Result()
			if err != nil {
				return body.Bytes(), err
			}
			for _, message := range messages {
				fields := []string{}
				for field := range message.Values {
					fields = append(fields, field)
				}
				sort.Strings(fields)
				write(message.ID)
				for _, field := range fields {
					write(field, fmt.Sprint(message.Values[field]))
				}
				after = "(" + message.ID
			}
			if int64(len(messages)) < settings.RedisRangeCount {
				break
			}
		}
	}
	return body.Bytes(), nil
}

// Every row of each table, each table an item of Postgres's JSON text of its rows, read a page at a time, each page
// in a read-only transaction, and held whole until the scan reads it.
func capturePostgres(ctx context.Context, tables []string, settings *CaptureStoresSettings, report *CaptureStoresReport) []*sidecartest.PrivacyItem {
	reader := &postgresReader{}
	present := []string{}
	for _, table := range tables {
		rows, err := reader.Rows(ctx, "SELECT to_regclass($1) IS NOT NULL", table)
		if err != nil {
			report.failed(fmt.Errorf("finding table %s: %w", table, err))
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
	items := []*sidecartest.PrivacyItem{}
	for _, table := range present {
		items = append(items, &sidecartest.PrivacyItem{
			Class: sidecartest.PrivacyClassPostgres,
			Name:  "postgres " + table,
			Open: func() (io.ReadCloser, error) {
				var body bytes.Buffer
				identifier := pgx.Identifier{table}.Sanitize()
				// ctid orders a page's rows, which no other column of every table can
				for offset := 0; ; offset += settings.PostgresPageRowCount {
					rows, err := reader.Rows(ctx, fmt.Sprintf("SELECT row_to_json(t)::text FROM %s t ORDER BY ctid LIMIT %d OFFSET %d", identifier, settings.PostgresPageRowCount, offset))
					if err != nil {
						report.failed(fmt.Errorf("reading table %s: %w", table, err))
						break
					}
					for _, row := range rows {
						if text, ok := row[0].(string); ok {
							body.WriteString(text + "\n")
						} else {
							encoded, _ := json.Marshal(row[0])
							body.Write(append(encoded, '\n'))
						}
					}
					report.stateLock.Lock()
					report.rowCount += len(rows)
					report.stateLock.Unlock()
					if len(rows) < settings.PostgresPageRowCount {
						break
					}
				}
				return io.NopCloser(&body), nil
			},
		})
	}
	return items
}
