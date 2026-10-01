// Downloads source data without placing provider credentials in argv or errors.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/geo"
	"gopkg.in/yaml.v3"
)

// The documented selective archive omits unrelated points of contact and ASNs.
const arinDownloadUrl = "https://accountws.arin.net/public/rest/downloads/bulkwhois/orgs+nets.zip"
const maximumArinArchiveBytes int64 = 4 * 1024 * 1024 * 1024
const maximumArinXmlBytes int64 = 16 * 1024 * 1024 * 1024
const maximumArinCredentialBytes int64 = 64 * 1024

type geoipUpdater func(context.Context, string, string) error

// The updater reads its own protected config. Its output can contain diagnostic
// URLs, so only the exit status is propagated to the release transcript.
func runGeoipUpdate(ctx context.Context, config string, directory string) error {
	return withMaxMindNativeConfig(ctx, config, func(nativeConfig string) error {
		command := exec.CommandContext(ctx, "geoipupdate", "-f", nativeConfig, "-d", directory)
		command.Stdout, command.Stderr = io.Discard, io.Discard
		return command.Run()
	})
}

// Downloads and verifies a City file, then exports its matching place list.
func refreshGeolite2(ctx context.Context, config string, output string, update geoipUpdater) error {
	if config == "" {
		return errors.New("geoip-config is required")
	}
	if err := update(ctx, config, output); err != nil {
		return err
	}
	path := filepath.Join(output, "GeoLite2-City.mmdb")
	db, err := server.OpenIpInfoDatabase(path)
	if err != nil {
		return fmt.Errorf("validate downloaded GeoLite2: %w", err)
	}
	buildTime := db.BuildTime()
	db.Close()
	if buildTime.After(time.Now().Add(24*time.Hour)) || buildTime.Before(time.Now().Add(-30*24*time.Hour)) {
		return errors.New("GeoLite2 source build date is outside the supported freshness window")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	places, err := geo.ReadMmdbExport(path)
	if err != nil {
		return fmt.Errorf("export GeoLite2 places: %w", err)
	}
	content, err := places.Marshal()
	if err != nil {
		return err
	}
	if _, err := geo.LoadPlaces(content); err != nil {
		return fmt.Errorf("validate GeoLite2 places: %w", err)
	}
	if err := writeSyncedFile(filepath.Join(output, "places.yml"), content); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(output, "geolite2.mmdb")); err != nil {
		return err
	}
	return writeManifest(output, map[string]any{"source": "GeoLite2-City", "source_build_time": buildTime.UTC(), "fetched_at": time.Now().UTC()}, "geolite2.mmdb", "places.yml")
}

// A dedicated YAML credential, read only by the downloader process.
type arinCredentials struct {
	ApiKey string `yaml:"api_key"`
}

// Bound the read itself, not only the resulting allocation. Parser diagnostics
// never escape because they can contain the rejected credential value.
func decodeArinCredentials(reader io.Reader) (arinCredentials, error) {
	var credential arinCredentials
	input, err := io.ReadAll(io.LimitReader(reader, maximumArinCredentialBytes+1))
	if err != nil {
		return credential, errors.New("ARIN credential file cannot be read")
	}
	if int64(len(input)) > maximumArinCredentialBytes {
		return credential, errors.New("ARIN credential file is too large")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(input))
	decoder.KnownFields(true)
	if err := decoder.Decode(&credential); err != nil {
		return arinCredentials{}, errors.New("ARIN credential file is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return arinCredentials{}, errors.New("ARIN credential file must contain one document")
	}
	credential.ApiKey = strings.TrimSpace(credential.ApiKey)
	if credential.ApiKey == "" || strings.ContainsAny(credential.ApiKey, "\r\n\t ") {
		return arinCredentials{}, errors.New("ARIN api_key is missing or malformed")
	}
	return credential, nil
}

// The HTTP client is injectable for deterministic transport tests. Redirects
// are refused so the query credential cannot cross to an unrelated authority.
func refreshArin(ctx context.Context, credentialsPath string, output string, supplied *http.Client) error {
	if credentialsPath == "" {
		return errors.New("credentials is required")
	}
	file, err := os.Open(credentialsPath)
	if err != nil {
		return errors.New("ARIN credential file cannot be read")
	}
	credential, err := decodeArinCredentials(file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return errors.New("ARIN credential file could not be closed")
	}
	endpoint, _ := url.Parse(arinDownloadUrl)
	query := endpoint.Query()
	query.Set("apikey", credential.ApiKey)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return errors.New("cannot construct ARIN download request")
	}
	client := http.Client{Timeout: 30 * time.Minute}
	if supplied != nil {
		client = *supplied
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("ARIN source download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("ARIN source download returned HTTP %d", response.StatusCode)
	}
	archive, err := os.CreateTemp(filepath.Dir(output), ".arin-download-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	n, copyErr := io.Copy(archive, io.LimitReader(response.Body, maximumArinArchiveBytes+1))
	closeErr = archive.Close()
	if copyErr != nil {
		return errors.New("ARIN source download was interrupted")
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maximumArinArchiveBytes {
		return errors.New("ARIN source archive exceeds size limit")
	}
	reader, err := zip.OpenReader(archive.Name())
	if err != nil {
		return errors.New("ARIN source is not a valid ZIP archive")
	}
	defer reader.Close()
	var source *zip.File
	for _, file := range reader.File {
		if filepath.Base(file.Name) != "arin_db.xml" {
			continue
		}
		if source != nil {
			return errors.New("ARIN archive has multiple source files")
		}
		source = file
	}
	if source == nil || source.UncompressedSize64 > uint64(maximumArinXmlBytes) {
		return errors.New("ARIN archive has no bounded XML source")
	}
	contents, err := source.Open()
	if err != nil {
		return errors.New("ARIN XML entry cannot be opened")
	}
	defer contents.Close()
	file, err = os.Create(output)
	if err != nil {
		return err
	}
	n, copyErr = io.Copy(file, io.LimitReader(contents, maximumArinXmlBytes+1))
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr = file.Close()
	if copyErr != nil {
		return errors.New("ARIN XML extraction failed its integrity check")
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maximumArinXmlBytes {
		return errors.New("ARIN XML exceeds size limit")
	}
	return scanArinXml(ctx, output, nil, nil)
}

// Every artifact is closed and synced before its containing directory is published.
func writeSyncedFile(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// Source versions and content hashes are non-secret release inputs.
func writeManifest(directory string, metadata map[string]any, names ...string) error {
	metadata["builder_version"] = Version
	hashes := map[string]string{}
	for _, name := range names {
		file, err := os.Open(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		hashes[name] = hex.EncodeToString(hash.Sum(nil))
	}
	metadata["sha256"] = hashes
	content, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return writeSyncedFile(filepath.Join(directory, "manifest.json"), append(content, '\n'))
}
