// `sidecarctl sockets ticket-keygen -out <file> [-in <file>]` writes the sockets service's ticket key file, vault's
// socket-ticket-keys.yml (§9.8, owner decision 354, QD-902). It is a thin wrapper over
// socketsservice.SocketTicketKeyFileAdd, which SOCK-M5-08 built for it. The owner runs it every RotationInterval (30
// days) on a machine of their own and places the file in vault, and each sockets block rereads it every
// ReloadInterval (a minute). It works on files alone: no env, no vault and no network.
//
//   - -in, when given, names the current file. Its keys still live at the run's time are kept as they are, so each
//     verifies through its own expiry, and the expired are dropped. A file the block would refuse (one that does
//     not parse, names a key twice or holds a seed of the wrong size), an empty one, or one past MaxFileByteCount
//     refuses the run, so a rotation never starts over from a file that lost its keys. Without -in the file is a
//     deployment's first.
//   - One new Ed25519 key is added, created at the run's time and expiring RotationInterval plus KeyOverlap (31
//     days) after it. It verifies once a block has read the file, and a block signs with it once it has settled
//     there, SettleTime (10 min) after the later of its creation and the first read in which that block saw it, by
//     when the block has given it to every listener. So however long the placement in vault takes, it only delays
//     the switch, and the output gives no time to place the file by (owner decision 562, QD-1016). A deployment's
//     first key, or one added when no key of -in can sign any more, signs as soon as a block reads the file, which
//     the output says.
//   - -out gets the file, mode 0600. A path that exists is written over only when -in names that same file, the
//     rotation in place; a symlink, or anything else that is not a regular file, at -out is refused. The bytes go into
//     a new file beside -out and are synced, then linked into place for a new path, which never writes over a file
//     that appeared meanwhile, or renamed over the old file; so -out holds the old file or the new one whole, never a
//     part. A run killed between the two can leave that file, <out>.<n>.tmp, which holds keys: remove it by hand.
//
// Before anything is written, the new file is read back by the sockets block's own key set at the run's time: it
// must hold every key -in held live, unchanged, and exactly one more, the new key, with its expiry. A flag's value
// may follow it apart, as the design writes the command, or after an =, as sidecarctl's other commands take theirs:
// with no positionals, neither can be taken for one. The output names keys by their ids, which GET /socket/keys
// publishes; no seed, private key or public key reaches stdout, stderr or a log.

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/socketsservice"
)

// vault's name for the key file, as server/cli/sockets mounts it (its hostTicketKeyFileName).
const socketsTicketKeyResourceName = "socket-ticket-keys.yml"

// The command's registration. It is the only change to the binary this command needs.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "sockets ticket-keygen",
		usage:   "-out <file> [-in <file>]",
		summary: defaultSocketsTicketKeygenSettings().summary(),
		run:     runSocketsTicketKeygen,
	})
}

// How a run reads, adds and writes. A caller changes its own copy; tests replace the clock and the add.
type socketsTicketKeygenSettings struct {
	// The key file's format and rotation, which the sockets block reads the file with:
	// socketsservice.DefaultSocketTicketKeySettings. Its MaxFileByteCount also bounds what is read of -in.
	ticketKeySettings *socketsservice.SocketTicketKeySettings
	// The mode the file is written with: its owner alone, as vault's files are.
	outputFileMode os.FileMode
	// The run's time: the new key's creation, and what -in's keys are judged live or expired at.
	now func() time.Time
	// What adds the key to a file's bytes: socketsservice.SocketTicketKeyFileAdd.
	addKey func(existing []byte, now time.Time, settings *socketsservice.SocketTicketKeySettings) ([]byte, error)
}

// The settings of a run.
func defaultSocketsTicketKeygenSettings() *socketsTicketKeygenSettings {
	return &socketsTicketKeygenSettings{
		ticketKeySettings: socketsservice.DefaultSocketTicketKeySettings(),
		outputFileMode:    0o600,
		now:               time.Now,
		addKey:            socketsservice.SocketTicketKeyFileAdd,
	}
}

// The command's line in the binary's help, its times from these settings.
func (self *socketsTicketKeygenSettings) summary() string {
	return fmt.Sprintf("Add a new ticket key to the sockets service's key file (§9.8, owner decision 354): -in names the "+
		"current file, whose live keys are kept and expired keys dropped; -out gets the new file, created %04o and never "+
		"over a file unless -in names it. It prints the new key's id and expiry, never a key. Run it every %s and place "+
		"the file in vault as %s, which each sockets block rereads every %s, and a block signs with a rotation's new "+
		"key %s after the first read in which it sees it.",
		self.outputFileMode.Perm(), socketsTicketKeygenDurationText(self.ticketKeySettings.RotationInterval),
		socketsTicketKeyResourceName, self.ticketKeySettings.ReloadInterval, self.ticketKeySettings.SettleTime)
}

// One file a flag names, and whether the flag was given.
type socketsTicketKeygenPath struct {
	// the flag, for messages
	flagName string
	value    string
	given    bool
}

// Takes the flag's value, refusing a second, which would leave the run's file a matter of order.
func (self *socketsTicketKeygenPath) set(value string) error {
	if self.given {
		return errors.New("given twice")
	}
	self.value = value
	self.given = true
	return nil
}

// `sidecarctl sockets ticket-keygen`.
func runSocketsTicketKeygen(args []string, out io.Writer) error {
	return runSocketsTicketKeygenWithSettings(args, defaultSocketsTicketKeygenSettings(), out)
}

// Parses the command line, reads -in, adds the key, reads the new file back and writes it, or writes nothing.
func runSocketsTicketKeygenWithSettings(args []string, settings *socketsTicketKeygenSettings, out io.Writer) error {
	if err := settings.ticketKeySettings.Validate(); err != nil {
		return err
	}
	outPath := &socketsTicketKeygenPath{flagName: "-out"}
	inPath := &socketsTicketKeygenPath{flagName: "-in"}
	flags := flag.NewFlagSet("sockets ticket-keygen", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Func("out", "", outPath.set)
	flags.Func("in", "", inPath.set)
	if err := flags.Parse(args); err != nil {
		return sidecarctlUsageErrorf("%v", err)
	}
	if 0 < flags.NArg() {
		return sidecarctlUsageErrorf("takes -out and -in alone; got %q", flags.Args())
	}
	if !outPath.given || outPath.value == "" {
		return sidecarctlUsageErrorf("-out names no file")
	}
	if inPath.given && inPath.value == "" {
		return sidecarctlUsageErrorf("-in names no file; for a deployment's first file leave -in off")
	}
	for _, path := range []*socketsTicketKeygenPath{outPath, inPath} {
		if strings.HasPrefix(path.value, "-") {
			// most likely a flag taken for the value of the flag before it
			return sidecarctlUsageErrorf("%s=%q looks like a flag; name a file beginning with - as ./%s", path.flagName, path.value, path.value)
		}
	}

	now := settings.now()
	replace, err := settings.checkOut(outPath.value, inPath)
	if err != nil {
		return err
	}
	var existing []byte
	if inPath.given {
		if existing, err = settings.readIn(inPath.value); err != nil {
			return err
		}
	}
	data, err := settings.addKey(existing, now, settings.ticketKeySettings)
	if err != nil {
		if inPath.given {
			return fmt.Errorf("nothing was written: -in=%s: %w", inPath.value, err)
		}
		return fmt.Errorf("nothing was written: %w", err)
	}
	keptKeys := []*sidecar.SocketTicketKey{}
	if inPath.given {
		if keptKeys, _, err = settings.readKeys(existing, now); err != nil {
			return fmt.Errorf("nothing was written: %w", err)
		}
	}
	writtenKeys, signingKeyId, err := settings.readKeys(data, now)
	if err != nil {
		return fmt.Errorf("nothing was written: %w", err)
	}
	newKey, err := settings.check(keptKeys, writtenKeys, now)
	if err != nil {
		return err
	}
	if err := settings.write(outPath.value, data, replace); err != nil {
		return err
	}

	fmt.Fprintf(out, "added ticket key %s: created %s, it verifies until %s\n",
		newKey.KeyId, socketsTicketKeygenTimeText(now), socketsTicketKeygenMillisText(newKey.ExpireTimeMillis))
	if inPath.given {
		fmt.Fprintf(out, "kept the keys of -in=%s still live at %s, each as it was (%d in all); any expired were dropped\n", inPath.value, socketsTicketKeygenTimeText(now), len(keptKeys))
		for _, key := range keptKeys {
			fmt.Fprintf(out, "kept ticket key %s, which verifies until %s\n", key.KeyId, socketsTicketKeygenMillisText(key.ExpireTimeMillis))
		}
	}
	// what a block first reading the file at the run's time signs with: a key of -in while one can sign
	switch {
	case signingKeyId != newKey.KeyId:
		fmt.Fprintf(out, "each sockets block signs with the new key %s after the first read in which it sees the file, and with a key of -in until then\n",
			settings.ticketKeySettings.SettleTime)
	case inPath.given:
		fmt.Fprintf(out, "no key of -in can sign any more, so the new key signs as soon as a sockets block reads the file, before its listeners have been given it\n")
	default:
		fmt.Fprintf(out, "the new key is the file's only one, so it signs as soon as a sockets block reads the file\n")
	}
	fmt.Fprintf(out, "wrote %s, readable by its owner alone; no key was printed\n", outPath.value)
	fmt.Fprintf(out, "place %s in vault as %s: each sockets block rereads it every %s and gives the new key to its listeners\n",
		outPath.value, socketsTicketKeyResourceName, settings.ticketKeySettings.ReloadInterval)
	fmt.Fprintf(out, "run this again by %s with -in naming this file, so its keys go on verifying while the next one settles\n",
		socketsTicketKeygenTimeText(now.Add(settings.ticketKeySettings.RotationInterval)))
	return nil
}

// Whether the run replaces the file at outPath. A path that exists is refused unless it is a regular file and -in
// names that same file, the rotation in place; a symlink at it is refused even then, since a rename would replace the
// link and leave the file it names as it was.
func (self *socketsTicketKeygenSettings) checkOut(outPath string, inPath *socketsTicketKeygenPath) (bool, error) {
	outInfo, err := os.Lstat(outPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("nothing was written: -out: %w", err)
	case outInfo.Mode()&os.ModeSymlink != 0:
		return false, fmt.Errorf("nothing was written: -out=%s is a symlink; name the file itself", outPath)
	case !outInfo.Mode().IsRegular():
		return false, fmt.Errorf("nothing was written: -out=%s is not a regular file", outPath)
	}
	if inPath.given {
		if inInfo, err := os.Stat(inPath.value); err == nil && os.SameFile(outInfo, inInfo) {
			return true, nil
		}
	}
	return false, fmt.Errorf("nothing was written: -out=%s exists, and this writes over a file only when -in names it", outPath)
}

// -in's bytes. A file larger than a key file may be, or an empty one, is refused: the sockets block reads either as
// no key file, so a rotation from it would start over with only the new key.
func (self *socketsTicketKeygenSettings) readIn(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("nothing was written: -in: %w", err)
	}
	defer file.Close()
	maxByteCount := int64(self.ticketKeySettings.MaxFileByteCount)
	data, err := io.ReadAll(io.LimitReader(file, maxByteCount+1))
	if err != nil {
		return nil, fmt.Errorf("nothing was written: -in: %w", err)
	}
	if maxByteCount < int64(len(data)) {
		return nil, fmt.Errorf("nothing was written: -in=%s holds more than the %d bytes a key file may", path, maxByteCount)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("nothing was written: -in=%s is empty, which is no key file; for a deployment's first file leave -in off", path)
	}
	return data, nil
}

// What a key file holds as the sockets block reads it at now, on a clock stopped there: the keys that verify, oldest
// first, and the id of the one that signs, empty when none does. The set first sees every key at now, so none has
// settled and the signer is the oldest that outlives SignMargin, as for a block first reading the file. A file the
// block refuses holds none.
func (self *socketsTicketKeygenSettings) readKeys(data []byte, now time.Time) ([]*sidecar.SocketTicketKey, string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	// ends the set's reread loop, which waits on a clock that never moves
	defer cancel()
	read := func() ([]byte, error) {
		return data, nil
	}
	keySet, err := socketsservice.NewSocketTicketKeySet(ctx, read, &socketsTicketKeygenStoppedClock{now: now}, nil, self.ticketKeySettings)
	if err != nil {
		return nil, "", err
	}
	signingKeyId, _, _ := keySet.SigningKey()
	return keySet.VerifyKeys(), signingKeyId, nil
}

// The new key, once the file read back holds each kept key unchanged and exactly one more, expiring
// RotationInterval and KeyOverlap after now. Anything else is a file the block should not be given. No message
// names more than a key's id.
func (self *socketsTicketKeygenSettings) check(keptKeys []*sidecar.SocketTicketKey, writtenKeys []*sidecar.SocketTicketKey, now time.Time) (*sidecar.SocketTicketKey, error) {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("nothing was written: the new file does not read back as the sockets block reads it: %s", fmt.Sprintf(format, args...))
	}
	writtenKeyIdKeys := map[string]*sidecar.SocketTicketKey{}
	for _, key := range writtenKeys {
		writtenKeyIdKeys[key.KeyId] = key
	}
	if len(writtenKeyIdKeys) != len(keptKeys)+1 {
		return nil, refuse("it holds a key count of %d where %d was due: -in's live keys and one new", len(writtenKeyIdKeys), len(keptKeys)+1)
	}
	for _, key := range keptKeys {
		writtenKey, ok := writtenKeyIdKeys[key.KeyId]
		if !ok || !bytes.Equal(writtenKey.PublicKey, key.PublicKey) || writtenKey.ExpireTimeMillis != key.ExpireTimeMillis {
			return nil, refuse("key %s of -in is not kept as it was", key.KeyId)
		}
		delete(writtenKeyIdKeys, key.KeyId)
	}
	var newKey *sidecar.SocketTicketKey
	for _, key := range writtenKeyIdKeys {
		newKey = key
	}
	expireMillis := now.Add(self.ticketKeySettings.RotationInterval + self.ticketKeySettings.KeyOverlap).UnixMilli()
	if newKey == nil || expireMillis < 0 || newKey.ExpireTimeMillis != uint64(expireMillis) {
		return nil, refuse("the new key does not expire %s after %s", self.ticketKeySettings.RotationInterval+self.ticketKeySettings.KeyOverlap, socketsTicketKeygenTimeText(now))
	}
	return newKey, nil
}

// Puts data at path whole or not at all. The bytes go into a new file beside path, given outputFileMode and synced,
// which is then linked at path when path is new, so a file that appeared there meanwhile is never written over, or
// renamed over it when replace says -in named it; the directory is synced after, so the name lasts a crash.
// Whatever happens, the file beside is gone when this returns.
func (self *socketsTicketKeygenSettings) write(path string, data []byte, replace bool) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("nothing was written: -out: %w", err)
	}
	besidePath := file.Name()
	// after a rename this finds nothing, and after a link path keeps the file
	defer os.Remove(besidePath)
	// a mode set apart from the create, which the umask cannot narrow
	err = file.Chmod(self.outputFileMode)
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("nothing was written: -out: %w", err)
	}
	if replace {
		err = os.Rename(besidePath, path)
	} else if err = os.Link(besidePath, path); errors.Is(err, os.ErrExist) {
		return fmt.Errorf("nothing was written: -out=%s appeared during the run, and this writes over a file only when -in names it", path)
	}
	if err != nil {
		return fmt.Errorf("nothing was written: -out: %w", err)
	}
	directoryFile, err := os.Open(directory)
	if err == nil {
		err = directoryFile.Sync()
		directoryFile.Close()
	}
	if err != nil {
		return fmt.Errorf("wrote -out=%s, but its directory was not synced, so a crash could still lose it: %w", path, err)
	}
	return nil
}

// A clock stopped at one time, which a read back reads a file's keys at. It never reaches a later time, so a wait on
// it ends only when its timeout is not positive. Safe for concurrent use.
type socketsTicketKeygenStoppedClock struct {
	now time.Time
}

// The time it stopped at.
func (self *socketsTicketKeygenStoppedClock) Now() time.Time {
	return self.now
}

// Fires at once for a timeout that is not positive, and otherwise never.
func (self *socketsTicketKeygenStoppedClock) After(timeout time.Duration) <-chan time.Time {
	fire := make(chan time.Time, 1)
	if timeout <= 0 {
		fire <- self.now
	}
	return fire
}

// Calls function at once, on its own goroutine, for a timeout that is not positive, and otherwise never.
func (self *socketsTicketKeygenStoppedClock) AfterFunc(timeout time.Duration, function func()) (stop func() bool) {
	if timeout <= 0 {
		go function()
		return func() bool {
			return false
		}
	}
	stopped := &atomic.Bool{}
	return func() bool {
		return stopped.CompareAndSwap(false, true)
	}
}

// A time as the output gives it: RFC 3339 in UTC.
func socketsTicketKeygenTimeText(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// A time in Unix milliseconds, as a key's expiry holds it, given as socketsTicketKeygenTimeText gives a time.
func socketsTicketKeygenMillisText(millis uint64) string {
	return socketsTicketKeygenTimeText(time.UnixMilli(int64(millis)))
}

// A duration as the help gives it: whole days as days, anything else as time prints it.
func socketsTicketKeygenDurationText(duration time.Duration) string {
	day := 24 * time.Hour
	switch {
	case duration == day:
		return "day"
	case 0 < duration && duration%day == 0:
		return fmt.Sprintf("%d days", duration/day)
	}
	return duration.String()
}
