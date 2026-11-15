package mxedgehost

// The IP listing feed's local mirror (MAIL-M7-20; owner decision 111, §15.4's Listings, §2.8 ext-4):
// mxedgeservice.MxListingSource over a directory a licensed feed's sync keeps current, so no sender
// address leaves for a lookup. It is the one adapter a feed's format needs.
//
//	files    one zone file per category, <category>.rbldnsd, the category a word of at most 12 B: the sync
//	         puts each list it pulls under the word the engine reads it by, "exploit" and "botnet" for the
//	         lists owner decision 111 rejects at the banner and any other word for those it passes on
//	format   rbldnsd's ip4set and ip6trie data as feeds deliver them: one address or CIDR prefix a line,
//	         IPv4 or IPv6, an optional value after it, which is read and not used, since the file names the
//	         category; blank lines, comments (# or ;), $ directives and :value default lines are skipped
//	refused  any line that is not one of these, an exclusion (!) above all: an exclusion the engine dropped
//	         would list what the feed does not, and a listing rejects mail, so a file the adapter cannot read
//	         whole fails the load, and the engine keeps its set until that goes stale, rejecting nothing
//	current  the oldest modification time among the files, the least fresh list, which the engine holds to
//	         ListingStaleTimeout: a sync that stopped leaves the mirror stale, and stale rejects nothing
//
// The pull itself, which holds the feed's credential from the block's secret_files, is not built: no feed is
// licensed yet (MAIL-M7-20's external need), and its transport is the feed's. With no pull the directory is
// whatever an outside sync last wrote there, and with no directory the router takes no source and rejects
// nothing, §15.4's feed-down case. Nothing here logs an address, a prefix or a line (I-13).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/sidecar/mxedgeservice"
)

// A zone file's suffix in the mirror.
const feedMirrorFileSuffix = ".rbldnsd"

// The mirror's tunables.
type FeedMirrorSettings struct {
	// The directory the feed's sync writes. Empty: no mirror.
	Directory string
	// The longest line read, past which the load fails: 1 KiB, far past an address, a prefix and a value.
	LineMaxByteCount int
	// The most zone files read, past which the load fails, as a bound on what a directory can make this
	// process read: 64, the engine's categories.
	FileMaxCount int
}

func DefaultFeedMirrorSettings() *FeedMirrorSettings {
	return &FeedMirrorSettings{
		LineMaxByteCount: 1024,
		FileMaxCount:     64,
	}
}

// See the file header. Safe for concurrent use.
type FeedMirrorSource struct {
	settings *FeedMirrorSettings
}

var _ mxedgeservice.MxListingSource = (*FeedMirrorSource)(nil)

// The source over settings' directory.
func NewFeedMirrorSource(settings *FeedMirrorSettings) (*FeedMirrorSource, error) {
	if settings == nil || settings.Directory == "" || settings.LineMaxByteCount <= 0 || settings.FileMaxCount <= 0 {
		return nil, errors.New("feed mirror: no directory, or a bound that is not positive")
	}
	return &FeedMirrorSource{settings: settings}, nil
}

var errFeedMirrorRefused = errors.New("feed mirror: a file the adapter cannot read whole")

// See mxedgeservice.MxListingSource: every zone file's entries under its file's category, current at the
// oldest file's modification time. No file at all is an error, a mirror never synced.
func (self *FeedMirrorSource) Load(ctx context.Context, add mxedgeservice.MxListingAdd) (time.Time, error) {
	entries, err := os.ReadDir(self.settings.Directory)
	if err != nil {
		return time.Time{}, fmt.Errorf("feed mirror: %w", err)
	}
	names := []string{}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), feedMirrorFileSuffix) {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return time.Time{}, errors.New("feed mirror: no zone file")
	}
	if self.settings.FileMaxCount < len(names) {
		return time.Time{}, fmt.Errorf("%w: more than %d files", errFeedMirrorRefused, self.settings.FileMaxCount)
	}
	var currentTime time.Time
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		category := strings.TrimSuffix(name, feedMirrorFileSuffix)
		modifiedTime, err := self.loadFile(ctx, filepath.Join(self.settings.Directory, name), category, add)
		if err != nil {
			return time.Time{}, err
		}
		if currentTime.IsZero() || modifiedTime.Before(currentTime) {
			currentTime = modifiedTime
		}
	}
	return currentTime, nil
}

// One zone file's entries, and its modification time.
func (self *FeedMirrorSource) loadFile(ctx context.Context, path string, category string, add mxedgeservice.MxListingAdd) (time.Time, error) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("feed mirror: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return time.Time{}, fmt.Errorf("feed mirror: %w", err)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, self.settings.LineMaxByteCount), self.settings.LineMaxByteCount)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber += 1
		if lineNumber%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return time.Time{}, err
			}
		}
		prefix, ok, err := parseFeedMirrorLine(scanner.Text())
		if err != nil {
			// the file and line, never the line's text
			return time.Time{}, fmt.Errorf("%w: %s line %d: %v", errFeedMirrorRefused, filepath.Base(path), lineNumber, err)
		}
		if !ok {
			continue
		}
		if err := add(prefix, category); err != nil {
			return time.Time{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return time.Time{}, fmt.Errorf("%w: %s: %v", errFeedMirrorRefused, filepath.Base(path), err)
	}
	return info.ModTime(), nil
}

// One line's prefix, false for a line that lists nothing, or an error for one the adapter will not read.
func parseFeedMirrorLine(line string) (netip.Prefix, bool, error) {
	line = strings.TrimSpace(line)
	switch {
	case line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, ";"), strings.HasPrefix(line, "$"), strings.HasPrefix(line, ":"):
		return netip.Prefix{}, false, nil
	case strings.HasPrefix(line, "!"):
		return netip.Prefix{}, false, errors.New("an exclusion")
	}
	field := line
	if end := strings.IndexAny(line, " \t"); 0 <= end {
		field = line[:end]
		rest := strings.TrimSpace(line[end:])
		if rest != "" && !strings.HasPrefix(rest, ":") && !strings.HasPrefix(rest, "#") && !strings.HasPrefix(rest, ";") {
			return netip.Prefix{}, false, errors.New("text after the address that is no value")
		}
	}
	if strings.Contains(field, "/") {
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return netip.Prefix{}, false, errors.New("a prefix that does not parse")
		}
		if prefix != prefix.Masked() {
			return netip.Prefix{}, false, errors.New("a prefix with host bits")
		}
		return prefix, true, nil
	}
	addr, err := netip.ParseAddr(field)
	if err != nil || addr.Zone() != "" {
		// rbldnsd's abbreviated forms and ranges included: a line the adapter does not read lists nothing
		return netip.Prefix{}, false, errors.New("an address that does not parse")
	}
	return netip.PrefixFrom(addr, addr.BitLen()), true, nil
}
