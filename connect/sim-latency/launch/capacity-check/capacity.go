// Read-only native blob accounting, independent of competition policy loading.
// This proves this host's configured storage access, not Api-host readiness,
// physical free space, successful retained writes or off-site object custody.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server"
)

// One invocation owns its deadline and approved namespace allocation.
type capacitySettings struct {
	Timeout         time.Duration // Positive bound for all remote reads together.
	AllocationBytes int64         // Operator allocation, not physical disk capacity.
	Bucket          string
	Prefix          string
}

// These are the previously approved diagnostic scope and finite read budget.
func defaultCapacitySettings() capacitySettings {
	return capacitySettings{Timeout: 90 * time.Second, AllocationBytes: 10_000_000_000_000,
		Bucket: "competition", Prefix: "blob/competition/"}
}

// Reject missing/local fallback before constructing the native Minio client.
// Copy configuration so selecting this bucket cannot alter another owner.
func newCapacityStore(config *server.BlobStoreConfig, present bool, bucket string) (server.BlobStore, error) {
	if !present || config == nil || config.Local || strings.TrimSpace(config.Authority) == "" || strings.EqualFold(strings.TrimSpace(config.Authority), "local") {
		return nil, errors.New("native remote blob configuration unavailable")
	}
	selected := *config
	selected.Bucket = bucket
	store, err := server.NewBlobStore(&selected)
	if err != nil {
		return nil, fmt.Errorf("native blob client unavailable (%T)", err)
	}
	return store, nil
}

// Native retention configuration checks are Head/Get only; listing streams
// every version and delete marker. Errors cannot qualify partial accounting.
func readCapacity(ctx context.Context, store server.BlobStore, prefix string, allocationBytes int64) (*server.BlobUsage, error) {
	retained, ok := store.(server.RetainedBlobStore)
	if !ok {
		return nil, errors.New("read-only retention check unavailable")
	}
	if err := retained.CheckRetention(ctx); err != nil {
		return nil, fmt.Errorf("read-only retention check failed (%T)", err)
	}
	usage, err := server.MeasureBlobVersionUsageAtPrefix(ctx, store, prefix, allocationBytes)
	if err != nil {
		return nil, fmt.Errorf("read-only version accounting failed (%T)", err)
	}
	if usage == nil || !usage.IncludesObjectVersions || usage.UsedBytes < 0 || usage.UsedBytes > allocationBytes {
		return nil, errors.New("version-aware allocated capacity proof refused")
	}
	return usage, nil
}

// The only write is one exclusive local receipt inside an existing private
// Root directory. No competition settings, controller, database or Api is used.
func run() error {
	if os.Geteuid() != 0 || len(os.Args) != 2 {
		return errors.New("Root and one private output directory required")
	}
	directory := os.Args[1]
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("private output directory required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != 0 {
		return errors.New("Root-owned output directory required")
	}
	settings := defaultCapacitySettings()
	config, present := server.LoadBlobStoreConfig()
	store, err := newCapacityStore(config, present, settings.Bucket)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), settings.Timeout)
	defer cancel()
	startedAt := time.Now().UTC()
	usage, err := readCapacity(ctx, store, settings.Prefix, settings.AllocationBytes)
	if err != nil {
		return err
	}
	authorityDigest := sha256.Sum256([]byte(store.Authority()))
	receipt := map[string]any{"schema": 1, "kind": "competition-native-version-capacity-proof",
		"read_only": true, "started_at": startedAt, "completed_at": time.Now().UTC(),
		"authority_sha256": hex.EncodeToString(authorityDigest[:]), "usage": usage,
		"allocation_source": "operator-approved decimal 10 TB allocation", "writes": 0,
		"retention_configuration_checked": true, "physical_free_space_proved": false,
		"retained_write_proved": false, "off_site_custody_proved": false, "api_host_readiness_proved": false}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	out, err := root.OpenFile("capacity.private.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := json.NewEncoder(out).Encode(receipt); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

// Fail closed without printing configuration or backend error contents.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
