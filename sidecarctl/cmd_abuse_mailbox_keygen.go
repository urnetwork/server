// `sidecarctl abuse mailbox-keygen --out=<abuse-mailbox.yml> --private-out=<file>` makes the keys of §21.10's two
// mailboxes, the abuse mailbox and the legal-request mailbox (QD-726, QD-832): the HPKE-XWing pair every record is
// sealed to, and QD-078's MAC key. The owner runs it on the machine that keeps the private key, which works on files
// alone: no env, no vault and no network.
//
//   - --out gets abuse-mailbox.yml for vault, exactly as controller.AbuseMailboxKeyFile writes it: the MAC key
//     (MacKeyByteCount bytes from crypto/rand) and the X-Wing public key, each in standard base64, and nothing else.
//     api and taskworker mount it and seal every record to its public key.
//   - --private-out gets one line, `abuse_mailbox_seal_private_key: <base64 of the 32-byte seed>`: the X-Wing private
//     key, as crypto/hpke's PrivateKey.Bytes gives it and controller.ParseAbuseMailboxSealPrivateKey reads it back at
//     §19.10's restore (ABUSE-M5-18). It never goes into vault, whose file refuses it, or onto a host that runs a
//     service: the owner keeps it offline beside the restore runbook.
//
// The pair comes from hpke.MLKEM768X25519().GenerateKey(). Before anything is written, both files' bytes are read
// back through controller's parsers, and a probe sealed to the read public key must open with the read private key,
// so a file that would not parse, or a pair that does not match, is never written. Both files are created 0600 and
// never over a file: a path that exists, a symlink included, refuses the run, as does a run naming one file twice.
// Both are created before either is written, and a failed write removes both, so a run leaves both files whole or
// neither. Neither key reaches stdout, stderr or a log: the output names the files alone.
//
// A rotation is the same command with new files, the new abuse-mailbox.yml replacing the old in vault. Records
// sealed before it open only with the old seed and verify only with the old MAC key, so each generation's private
// file is kept offline with the abuse-mailbox.yml made beside it until the last record sealed to them has passed its
// ttl (§2.8's api-14: an action's record as long as the action is kept, a hold's until its expire_time).

package main

import (
	"bytes"
	"crypto/hpke"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urnetwork/server/controller"
)

// What the check before writing seals to the new public key and opens with the new private key.
const abuseMailboxKeygenProbe = "sidecarctl abuse mailbox-keygen probe"

// The command's registration. It is the only change to the binary this command needs.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words: "abuse mailbox-keygen",
		usage: "--out=<abuse-mailbox.yml> --private-out=<file>",
		summary: "Make the abuse and legal-request mailboxes' keys (QD-832): --out gets abuse-mailbox.yml for vault (the MAC key " +
			"and the X-Wing public key), --private-out the X-Wing private key's seed, which the owner keeps offline beside " +
			"§19.10's restore runbook and never puts in vault. Both are created 0600, never over a file, and neither key is " +
			"printed. A rotation is the same command with new files: keep every old private file, with the abuse-mailbox.yml " +
			"made beside it, until the last record sealed to them has passed its ttl.",
		run: runAbuseMailboxKeygen,
	})
}

// How the key generator draws and writes. A caller changes its own copy; tests replace the random source.
type abuseMailboxKeygenSettings struct {
	// The mailboxes' format: MacKeyByteCount sizes the MAC key, and the check before writing parses and seals with
	// it. controller.DefaultAbuseMailboxSettings, the values api, taskworker and the restore read with.
	mailboxSettings *controller.AbuseMailboxSettings
	// The mode both files are created with: their owner alone may read them, as vault's files are.
	outputFileMode os.FileMode
	// Where the MAC key's bytes come from: crypto/rand. The X-Wing pair is always drawn inside crypto/hpke.
	random io.Reader
}

// The settings of a run.
func defaultAbuseMailboxKeygenSettings() *abuseMailboxKeygenSettings {
	return &abuseMailboxKeygenSettings{
		mailboxSettings: controller.DefaultAbuseMailboxSettings(),
		outputFileMode:  0o600,
		random:          rand.Reader,
	}
}

// One file a run writes.
type abuseMailboxKeygenFile struct {
	// the flag that named it, for messages
	flagName string
	path     string
	// what it says the file is, for the output
	description string
	data        []byte
}

// `sidecarctl abuse mailbox-keygen`.
func runAbuseMailboxKeygen(args []string, out io.Writer) error {
	return runAbuseMailboxKeygenWithSettings(args, defaultAbuseMailboxKeygenSettings(), out)
}

// Parses the command line, draws the keys, checks the two files' bytes and writes both, or neither.
func runAbuseMailboxKeygenWithSettings(args []string, settings *abuseMailboxKeygenSettings, out io.Writer) error {
	flags := flag.NewFlagSet("abuse mailbox-keygen", flag.ContinueOnError)
	outFlag := flags.String("out", "", "")
	privateOutFlag := flags.String("private-out", "", "")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		// a value standing apart from its flag lands here, as in -out file: a value follows its flag's =
		return sidecarctlUsageErrorf("takes flags only, each value after an =, as --out=<file>; got %q", positionals)
	}
	for _, path := range []struct {
		flagName string
		value    string
	}{
		{flagName: "--out", value: *outFlag},
		{flagName: "--private-out", value: *privateOutFlag},
	} {
		if path.value == "" {
			return sidecarctlUsageErrorf("%s names no file", path.flagName)
		}
		if strings.HasPrefix(path.value, "-") {
			// most likely a flag taken for the value of one written without its =
			return sidecarctlUsageErrorf("%s=%q looks like a flag; name a file beginning with - as ./%s", path.flagName, path.value, path.value)
		}
	}
	outAbsPath, err := filepath.Abs(*outFlag)
	if err != nil {
		return err
	}
	privateOutAbsPath, err := filepath.Abs(*privateOutFlag)
	if err != nil {
		return err
	}
	if outAbsPath == privateOutAbsPath {
		return sidecarctlUsageErrorf("--out and --private-out name one file, and the private key never shares a file")
	}

	keys, privateKey, err := settings.draw()
	if err != nil {
		return fmt.Errorf("nothing was written: %w", err)
	}
	keyFile := controller.AbuseMailboxKeyFile(keys)
	privateKeyFile, err := controller.AbuseMailboxSealPrivateKeyFile(privateKey)
	if err != nil {
		return fmt.Errorf("nothing was written: %w", err)
	}
	if err := settings.check(keys, keyFile, privateKeyFile); err != nil {
		return fmt.Errorf("nothing was written: %w", err)
	}
	keyFiles := []*abuseMailboxKeygenFile{
		{
			flagName:    "--private-out",
			path:        *privateOutFlag,
			description: "the X-Wing private key's seed, which alone opens what is sealed to the public key",
			data:        privateKeyFile,
		},
		{
			flagName:    "--out",
			path:        *outFlag,
			description: "abuse-mailbox.yml for vault: the MAC key and the X-Wing public key",
			data:        keyFile,
		},
	}
	if err := settings.write(keyFiles); err != nil {
		return err
	}

	for _, keyFile := range keyFiles {
		fmt.Fprintf(out, "wrote %s, readable by its owner alone: %s\n", keyFile.path, keyFile.description)
	}
	fmt.Fprintf(out, "place %s in vault as %s for api and taskworker, which mount it; nothing else goes into that file\n", *outFlag, controller.AbuseMailboxKeysResourceName)
	fmt.Fprintf(out, "keep %s offline beside §19.10's restore runbook: never in vault, never on a host that runs a service; the restore opens the mailboxes' records with it\n", *privateOutFlag)
	fmt.Fprintf(out, "a rotation is this command again with new files: records sealed before it open only with this private file and verify only with this %s's MAC key, so keep a copy of both offline until the last record sealed to them has passed its ttl (§2.8's api-14)\n", controller.AbuseMailboxKeysResourceName)
	return nil
}

// Draws the X-Wing pair inside crypto/hpke and the MAC key from the settings' random source.
func (self *abuseMailboxKeygenSettings) draw() (*controller.AbuseMailboxKeys, hpke.PrivateKey, error) {
	privateKey, err := hpke.MLKEM768X25519().GenerateKey()
	if err != nil {
		return nil, nil, fmt.Errorf("no X-Wing pair was drawn: %w", err)
	}
	macKey := make([]byte, self.mailboxSettings.MacKeyByteCount)
	if _, err := io.ReadFull(self.random, macKey); err != nil {
		return nil, nil, fmt.Errorf("no MAC key was drawn: %w", err)
	}
	keys := &controller.AbuseMailboxKeys{
		MacKey:        macKey,
		SealPublicKey: privateKey.PublicKey(),
	}
	return keys, privateKey, nil
}

// Reads both files' bytes back as api, taskworker and the restore will, and opens a probe sealed to the read public
// key with the read private key, so a file that would not parse, or a pair that does not match, is never written.
// No error names a key: controller's never do, and this one's own say only what failed.
func (self *abuseMailboxKeygenSettings) check(keys *controller.AbuseMailboxKeys, keyFile []byte, privateKeyFile []byte) error {
	parsedKeys, err := controller.ParseAbuseMailboxKeys(keyFile, self.mailboxSettings)
	if err != nil {
		return err
	}
	if !bytes.Equal(parsedKeys.MacKey, keys.MacKey) || !bytes.Equal(parsedKeys.SealPublicKey.Bytes(), keys.SealPublicKey.Bytes()) {
		return fmt.Errorf("%s does not read back to the keys drawn", controller.AbuseMailboxKeysResourceName)
	}
	parsedPrivateKey, err := controller.ParseAbuseMailboxSealPrivateKey(privateKeyFile)
	if err != nil {
		return err
	}
	probe := []byte(abuseMailboxKeygenProbe)
	sealed, err := controller.SealAbuseMailboxRecord(parsedKeys.SealPublicKey, probe, self.mailboxSettings)
	if err != nil {
		return err
	}
	opened, err := controller.OpenAbuseMailboxRecord(parsedPrivateKey, sealed, self.mailboxSettings)
	if err != nil || !bytes.Equal(opened, probe) {
		return errors.New("a probe sealed to the public key does not open with the private key")
	}
	return nil
}

// Creates every file before writing any, so a path that exists refuses the run before a key's bytes reach the disk,
// then writes, syncs and closes each in order. Whatever fails, every file this created is removed, so the paths hold
// every file whole or none of them. The caller puts the private file first: a run killed between the two writes
// then leaves the public file empty, which vault's parser refuses, never a whole public file whose private key was
// not kept, to which api would seal records that nothing opens.
func (self *abuseMailboxKeygenSettings) write(keyFiles []*abuseMailboxKeygenFile) (returnErr error) {
	// a file is nil here once closed
	createdFiles := []*os.File{}
	createdPaths := []string{}
	defer func() {
		for _, file := range createdFiles {
			if file != nil {
				file.Close()
			}
		}
		if returnErr != nil {
			for _, path := range createdPaths {
				os.Remove(path)
			}
		}
	}()
	for _, keyFile := range keyFiles {
		file, err := os.OpenFile(keyFile.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, self.outputFileMode)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("nothing was written: %s=%s exists, and this never writes over a file", keyFile.flagName, keyFile.path)
		}
		if err != nil {
			return fmt.Errorf("nothing was written: %s: %w", keyFile.flagName, err)
		}
		createdFiles = append(createdFiles, file)
		createdPaths = append(createdPaths, keyFile.path)
	}
	for index, keyFile := range keyFiles {
		file := createdFiles[index]
		_, err := file.Write(keyFile.data)
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		createdFiles[index] = nil
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("nothing was written: %s: %w", keyFile.flagName, err)
		}
	}
	return nil
}
