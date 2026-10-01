package stats

// export the last N days of stats segments from minio into a self-describing
// tarball (goal 10). The tarball carries the raw `.pb.zst` segments under
// their object keys, a manifest, the sample.proto schema, and a README, so a
// competitor can decode it without the server source.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/stats/sample"
)

type exportManifest struct {
	GeneratedUnixMilli int64    `json:"generated_unix_milli"`
	Env                string   `json:"env"`
	Days               int      `json:"days"`
	Bucket             string   `json:"bucket"`
	Prefix             string   `json:"prefix"`
	Streams            []string `json:"streams"`
	SegmentKeys        []string `json:"segment_keys"`
	SegmentCount       int      `json:"segment_count"`
	TotalBytes         int64    `json:"total_bytes"`
}

// ExportSamples downloads the segments whose date partition falls in the last
// `days` days (UTC) and writes a .tgz to outPath. nowUnixMilli anchors the
// window (pass server.NowUtc().UnixMilli()). On any error — including a
// close/flush failure, which is a truncated archive, not a success — the
// partial output file is removed.
func ExportSamples(ctx context.Context, store server.BlobStore, env string, days int, nowUnixMilli int64, outPath string) error {
	outFile, err := os.Create(outPath)
	if err != nil {
		return err
	}
	err = writeExportArchive(ctx, store, env, days, nowUnixMilli, outFile)
	if closeErr := outFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(outPath)
		return err
	}
	return nil
}

// writeExportArchive writes the whole gzip'd tar archive to out. The tar and
// gzip layers are closed in order (inner flushes into outer) and every close
// error is propagated — a defer-and-discard here turned a disk-full at flush
// into a truncated .tgz reported as success (same discipline as
// FlatWriter.Close).
func writeExportArchive(ctx context.Context, store server.BlobStore, env string, days int, nowUnixMilli int64, out io.Writer) error {
	if days <= 0 {
		days = 7
	}

	// the set of date partitions in range
	now := time.UnixMilli(nowUnixMilli).UTC()
	inRange := map[string]bool{}
	for d := 0; d < days; d += 1 {
		inRange[now.AddDate(0, 0, -d).Format("2006-01-02")] = true
	}

	gzipWriter := gzip.NewWriter(out)
	tarWriter := tar.NewWriter(gzipWriter)

	err := func() error {
		manifest := exportManifest{
			GeneratedUnixMilli: nowUnixMilli,
			Env:                env,
			Days:               days,
			Bucket:             store.Bucket(),
			Prefix:             store.Prefix(),
		}
		streamSet := map[string]bool{}

		listPrefix := store.Prefix() + "/" + env + "/"
		objects, err := store.List(ctx, listPrefix)
		if err != nil {
			return err
		}
		for _, object := range objects {
			stream, day, ok := parseObjectKey(store.Prefix(), env, object.Key)
			if !ok || !inRange[day] {
				continue
			}
			streamSet[stream] = true

			if err := copyObjectToTar(ctx, store, object.Key, object.Size, tarWriter); err != nil {
				return err
			}
			manifest.SegmentKeys = append(manifest.SegmentKeys, object.Key)
			manifest.TotalBytes += object.Size
		}
		manifest.SegmentCount = len(manifest.SegmentKeys)
		for stream := range streamSet {
			manifest.Streams = append(manifest.Streams, stream)
		}

		// manifest, proto schema, and README
		manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
		if err := writeTarFile(tarWriter, "manifest.json", manifestBytes); err != nil {
			return err
		}
		if err := writeTarFile(tarWriter, "sample.proto", []byte(sample.ProtoSource)); err != nil {
			return err
		}
		return writeTarFile(tarWriter, "README.md", []byte(exportReadme(&manifest)))
	}()

	if closeErr := tarWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gzipWriter.Close(); err == nil {
		err = closeErr
	}
	return err
}

// ExportSamplesFlat is ExportSamples' single-file sibling: it downloads the
// same window of segments and writes them as one xz-compressed
// varint-delimited protobuf stream (see flat.go), the format handed to
// competitors. Returns the frame count.
func ExportSamplesFlat(ctx context.Context, store server.BlobStore, env string, days int, nowUnixMilli int64, outPath string) (int, error) {
	if days <= 0 {
		days = 7
	}

	now := time.UnixMilli(nowUnixMilli).UTC()
	inRange := map[string]bool{}
	for d := 0; d < days; d += 1 {
		inRange[now.AddDate(0, 0, -d).Format("2006-01-02")] = true
	}

	writer, err := NewFlatWriter(outPath)
	if err != nil {
		return 0, err
	}

	err = func() error {
		listPrefix := store.Prefix() + "/" + env + "/"
		objects, err := store.List(ctx, listPrefix)
		if err != nil {
			return err
		}
		for _, object := range objects {
			_, day, ok := parseObjectKey(store.Prefix(), env, object.Key)
			if !ok || !inRange[day] {
				continue
			}
			reader, err := store.Get(ctx, object.Key)
			if err != nil {
				return err
			}
			err = AppendSegmentTo(writer, reader)
			reader.Close()
			if err != nil {
				return err
			}
		}
		return nil
	}()

	// close exactly once, propagate the first error, and never leave a
	// partial flat file behind on failure
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(outPath)
		return 0, err
	}
	return writer.Count(), nil
}

func copyObjectToTar(ctx context.Context, store server.BlobStore, key string, size int64, tarWriter *tar.Writer) error {
	reader, err := store.Get(ctx, key)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: key,
		Mode: 0o644,
		Size: size,
	}); err != nil {
		return err
	}
	_, err = io.CopyN(tarWriter, reader, size)
	return err
}

func writeTarFile(tarWriter *tar.Writer, name string, content []byte) error {
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		return err
	}
	_, err := tarWriter.Write(content)
	return err
}

// parseObjectKey extracts stream and date from <prefix>/<env>/<stream>/<day>/…
func parseObjectKey(prefix string, env string, key string) (stream string, day string, ok bool) {
	want := prefix + "/" + env + "/"
	if !strings.HasPrefix(key, want) {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, want)
	parts := strings.Split(rest, "/")
	if len(parts) < 4 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func exportReadme(manifest *exportManifest) string {
	return fmt.Sprintf(`# urnetwork stats export

Generated: %s UTC
Env: %s
Window: last %d days
Segments: %d (%d bytes)
Streams: %s

## Layout

Each %s/<stream>/<yyyy-mm-dd>/<instance>/<segment>.pb.zst is a zstd stream of
length-delimited protobuf frames: a 4-byte big-endian length prefix followed by
that many bytes of a protobuf message. Decode with sample.proto (included).

The findproviders2 stream carries one FindProviders2Sample per matchmaking
call: the loaded candidate pool in scaled-weight order and the chosen client
ids, so the sampling, sorting, and selection can be traced.

## Anonymization

Client and network ids are stable pseudonyms, not production ids: the same real
id always maps to the same 16-byte value across every sample (so an entity can
be traced through the corpus), but the mapping cannot be reversed to a
production id. Caller identity is reduced to a country code; no ip addresses are
present.

## Decoding

    protoc --go_out=. sample.proto   # or your language of choice
    # then, per segment: zstd -d, and read <len:uint32be><frame> repeatedly
`,
		time.UnixMilli(manifest.GeneratedUnixMilli).UTC().Format(time.RFC3339),
		manifest.Env,
		manifest.Days,
		manifest.SegmentCount,
		manifest.TotalBytes,
		strings.Join(manifest.Streams, ", "),
		manifest.Prefix,
	)
}
