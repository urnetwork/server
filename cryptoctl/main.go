// Updates the OFAC crypto-address table that cryptosanctions screens payments
// against (mmm/BTCPAY.md §17.5).
//
//	cryptoctl update --output DIR [--source SDN.XML] [--url URL] [--timeout 10m]
//
// update reads OFAC's SDN list and writes the table into the new directory DIR
// (a release build passes config/all/crypto/<release>): addresses.json with
// each asset's addresses and manifest.json with the source's URL, OFAC's
// publish date and record count, the SHA-256 of the source, the per-asset
// counts and the digest of addresses.json (internal/cryptosanctionslist). DIR
// is staged beside itself and renamed into place only once the table reads
// back, and an existing directory is never replaced. On success update prints
// one summary line on stdout; on any failure it writes nothing and exits 1.
//
// The source is the SDN list's XML export from OFAC's Sanctions List Service:
//
//	https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.XML
//
// It is the static link the service's own download page builds for SDN.XML,
// and the legacy https://www.treasury.gov/ofac/downloads/sdn.xml redirects to
// it; the service answers with a redirect to the current publication. SDN.XML
// is OFAC's long-standing XML schema. Its Digital Currency Address identifiers
// are the same as SDN_ADVANCED.XML's (1077 distinct on 2026-11-09), and its
// publshInformation block carries the publish date and the number of entries,
// which must match the entries read. A source with no Digital Currency Address
// identifier at all, or no Bitcoin address, is refused as a schema change.
//
// Every "Digital Currency Address - <code>" identifier goes to each asset whose
// chain accepts it as an address, whatever OFAC's code: OFAC lists Omni USDT
// under USDT on Bitcoin addresses, BCH and BSV legacy addresses that are the
// same strings on Bitcoin, and EVM addresses under USDT, USDC, ARB, BSC and
// ETC that are the same Ethereum accounts. sol and usdc_solana take every
// Solana address. An identifier under an asset's own code (XBT, ETH, SOL, XMR,
// ZEC, TAO) that its chain does not accept is reported in the manifest's
// unrecognized list and the summary: OFAC has listed a TRON address under XBT
// and a 64-digit hex value under XMR.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server/internal/cryptosanctionslist"
)

// The release build stamps this with its version.
var Version = "development"

// OFAC's SDN.XML export (see the package comment).
const DefaultSourceUrl = "https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.XML"

// SDN.XML is about 30 MB; this bounds a wrong response.
const maxSourceBytes = 512 * 1024 * 1024

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runCommand(ctx, os.Args[1:], os.Stdout, os.Stderr, commandDependencies{}); err != nil {
		fmt.Fprintln(os.Stderr, "cryptoctl:", err)
		os.Exit(1)
	}
}

// Dependencies belong to one command, so tests never replace a global client.
type commandDependencies struct {
	// nil means a client of its own
	httpClient *http.Client
	// nil means time.Now
	now func() time.Time
	// nil means the real waits between download attempts
	retryDelay func(attempt int) time.Duration
}

func (self commandDependencies) clock() time.Time {
	if self.now != nil {
		return self.now().UTC()
	}
	return time.Now().UTC()
}

func (self commandDependencies) client() *http.Client {
	if self.httpClient != nil {
		return self.httpClient
	}
	return &http.Client{}
}

func (self commandDependencies) delay(attempt int) time.Duration {
	if self.retryDelay != nil {
		return self.retryDelay(attempt)
	}
	return time.Duration(attempt) * 15 * time.Second
}

type updateOptions struct {
	output   string
	source   string
	url      string
	timeout  time.Duration
	attempts int
}

func runCommand(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, dependencies commandDependencies) error {
	if len(args) == 0 || args[0] != "update" {
		return errors.New("usage: cryptoctl update --output DIR [--source SDN.XML] [--url URL] [--timeout 10m] [--attempts 3]")
	}
	options := updateOptions{}
	flags := flag.NewFlagSet("cryptoctl update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.output, "output", "", "new table directory, as config/all/crypto/<release>; an existing directory is never replaced")
	flags.StringVar(&options.source, "source", "", "an SDN.XML file to read instead of downloading")
	flags.StringVar(&options.url, "url", DefaultSourceUrl, "the SDN.XML export to download")
	flags.DurationVar(&options.timeout, "timeout", 10*time.Minute, "total command deadline")
	flags.IntVar(&options.attempts, "attempts", 3, "download attempts")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || options.output == "" || options.timeout <= 0 || options.attempts < 1 {
		return errors.New("update needs --output, a positive --timeout and --attempts, and no positional arguments")
	}
	urlSet := false
	flags.Visit(func(set *flag.Flag) {
		urlSet = urlSet || set.Name == "url"
	})
	if options.source != "" && urlSet {
		return errors.New("--source and --url name two sources")
	}
	ctx, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()
	summary, err := runUpdate(ctx, options, dependencies)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, summary)
	return nil
}

// Reads the source, builds the table and publishes it at options.output.
func runUpdate(ctx context.Context, options updateOptions, dependencies commandDependencies) (string, error) {
	output, err := filepath.Abs(options.output)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(output); err == nil {
		return "", fmt.Errorf("%s already exists; a table is written to a new directory", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	source, err := readSource(ctx, options, dependencies)
	if err != nil {
		return "", err
	}
	defer source.close()

	publication, err := parseSdnList(source.file)
	if err != nil {
		return "", fmt.Errorf("%s: %w", source.url, err)
	}
	table := classify(publication)
	if len(table.entries[cryptosanctionslist.AssetBtc]) == 0 {
		return "", fmt.Errorf("%s: no Bitcoin address among %d Digital Currency Address identifiers; refusing a table without one", source.url, len(publication.ids))
	}
	manifest := cryptosanctionslist.Manifest{
		Generator: "cryptoctl " + Version,
		Source: cryptosanctionslist.Source{
			Url:         source.url,
			PublishDate: publication.publishDate,
			RecordCount: publication.recordCount,
			Sha256:      source.sha256,
			Bytes:       source.bytes,
			RetrievedAt: source.retrievedAt,
		},
		OfacDigitalCurrencyIds: table.ofacCurrencyIds,
		Unrecognized:           table.unrecognized,
	}
	err = publishDirectory(output, func(stage string) error {
		if err := cryptosanctionslist.WriteTable(stage, manifest, table.entries); err != nil {
			return err
		}
		// what a process will read, verified before it is published
		written, err := cryptosanctionslist.ReadTable(stage)
		if err != nil {
			return fmt.Errorf("the written table does not read back: %w", err)
		}
		if 0 < len(written.Skipped) {
			return fmt.Errorf("the written table reads back with skipped entries: %s", strings.Join(written.Skipped, "; "))
		}
		for _, asset := range cryptosanctionslist.Assets() {
			if written.Count(asset) != len(table.entries[asset]) {
				return fmt.Errorf("the written table reads back %d %s addresses, want %d", written.Count(asset), asset, len(table.entries[asset]))
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return summarize(manifest, table), nil
}

// The one line a release message carries.
func summarize(manifest cryptosanctionslist.Manifest, table classifiedTable) string {
	counts := []string{}
	for _, asset := range cryptosanctionslist.Assets() {
		counts = append(counts, fmt.Sprintf("%s %d", asset, len(table.entries[asset])))
	}
	total := 0
	for _, count := range table.ofacCurrencyIds {
		total += count
	}
	summary := fmt.Sprintf(
		"OFAC SDN list of %s (sha256 %s, %d digital currency identifiers): %s",
		manifest.Source.PublishDate,
		manifest.Source.Sha256[:12],
		total,
		strings.Join(counts, ", "),
	)
	if 0 < len(table.unrecognized) {
		notes := []string{}
		for _, unrecognized := range table.unrecognized {
			notes = append(notes, fmt.Sprintf("%s %s %s (SDN %d)", unrecognized.Asset, unrecognized.OfacCurrency, unrecognized.Address, unrecognized.SdnUid))
		}
		summary += fmt.Sprintf("; %d not valid on their chain and left out: %s", len(table.unrecognized), strings.Join(notes, ", "))
	}
	return summary
}

// The source as read, in a private temporary file.
type sourceFile struct {
	url         string
	file        *os.File
	sha256      string
	bytes       int64
	retrievedAt time.Time
	temporary   bool
}

func (self *sourceFile) close() {
	self.file.Close()
	if self.temporary {
		os.Remove(self.file.Name())
	}
}

func readSource(ctx context.Context, options updateOptions, dependencies commandDependencies) (*sourceFile, error) {
	if options.source != "" {
		path, err := filepath.Abs(options.source)
		if err != nil {
			return nil, err
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		size, err := io.Copy(hash, io.LimitReader(file, maxSourceBytes+1))
		if err == nil && maxSourceBytes < size {
			err = fmt.Errorf("%s exceeds %d bytes", path, maxSourceBytes)
		}
		if err == nil {
			_, err = file.Seek(0, io.SeekStart)
		}
		if err != nil {
			file.Close()
			return nil, err
		}
		return &sourceFile{
			url:         (&url.URL{Scheme: "file", Path: path}).String(),
			file:        file,
			sha256:      hex.EncodeToString(hash.Sum(nil)),
			bytes:       size,
			retrievedAt: dependencies.clock(),
		}, nil
	}
	var lastErr error
	for attempt := 1; attempt <= options.attempts; attempt += 1 {
		if 1 < attempt {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%w (after %v)", ctx.Err(), lastErr)
			case <-time.After(dependencies.delay(attempt - 1)):
			}
		}
		source, err := download(ctx, options.url, dependencies)
		if err == nil {
			return source, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("download %s failed %d times: %w", options.url, options.attempts, lastErr)
}

// One GET, following the service's redirect, into a temporary file. A short
// body, a body past the declared length and a non-200 answer are failures.
func download(ctx context.Context, sourceUrl string, dependencies commandDependencies) (*sourceFile, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceUrl, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "urnetwork-cryptoctl/"+Version)
	response, err := dependencies.client().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", sourceUrl, response.Status)
	}
	file, err := os.CreateTemp("", "cryptoctl-sdn-*.xml")
	if err != nil {
		return nil, err
	}
	source := &sourceFile{url: sourceUrl, file: file, temporary: true}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxSourceBytes+1))
	if err == nil && maxSourceBytes < size {
		err = fmt.Errorf("%s sent more than %d bytes", sourceUrl, maxSourceBytes)
	}
	if err == nil && 0 <= response.ContentLength && size != response.ContentLength {
		err = fmt.Errorf("%s sent %d of %d bytes", sourceUrl, size, response.ContentLength)
	}
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		source.close()
		return nil, err
	}
	source.sha256 = hex.EncodeToString(hash.Sum(nil))
	source.bytes = size
	source.retrievedAt = dependencies.clock()
	return source, nil
}

// Builds the directory in a stage beside output and renames it into place,
// so a process never sees a partial table. The stage's name is not a version,
// so the resolver never selects it.
func publishDirectory(output string, build func(stage string) error) error {
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".cryptoctl-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := build(stage); err != nil {
		return err
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("%s appeared during the update", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(stage, output)
}
