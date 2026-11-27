// `sidecarctl compute policy tickets sign|release` (ATTEST-M12-02; DESIGN-COMPUTE §5.4's freshness tickets, D-ATTEST-5,
// IC-9; compute-plan-ticket-distribution, QD-1379): a sitting's batch of daily tickets, and the ticket host's release
// of them, on the builder sidecar's root gives (sidecar/attest_policy_build.go):
//
//   - tickets sign --store=<dir> --key=<name-cert.pub> --policy=<version> [--revocation] [--expires=<rfc3339>]
//     [--staging] builds the sitting's batch, one ticket a day from the day after the sitting until its expiry, at
//     most 14 days on, or, with --revocation, a first ticket starting at the sitting, each bounded in UTC inside its
//     signature; signs each as sign signs a policy, under the same namespace by the same certified key through
//     ssh-keygen -Y sign on the holder's token, refusing a certificate that ends before the batch does; checks each
//     as a device would; and writes the batch whole into <store>/<batch id>/<yyyy-mm-dd>.bin, never half of it;
//   - tickets release --store=<dir> --tree=<dir> [--staging] is what the ticket host runs on its own schedule: from its
//     store of unreleased tickets it takes the newest batch whose sitting has come, deletes every older batch's
//     tickets, all of them unreleased since the store keeps none it published, and publishes each of the newest
//     batch's tickets once its not_before has come, into the serving tree as tickets/<yyyy-mm-dd>.bin, the path every
//     device's fetcher reads, then deletes it from the store. A ticket not yet started is never published, one whose
//     window ended unreleased is deleted, and one naming a policy version the tree does not serve yet is held back,
//     since it would stop remote work under the policy devices hold until they could fetch one that is not there.
//
// The store is the capability a revocation must defeat (§5.4): release deletes an older batch's tickets, never only
// hides them, and keeps nothing it has published, so a breach of the host holds only what is still to come, and a
// breach of the serving path only what is released. The host's own account, keys and deploy are an operations
// deployment (External); the serving tree is a directory now, mmm's publishing path posting it on, as QD-1379 has
// the ticket host post through the policy's own path. Every check pins this build's release key, none until the key
// ceremony, so until then nothing signs and nothing in a store verifies, which is the fail-closed answer.

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"
)

// The commands' registration. It is the only change to the binary they need.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute policy tickets sign",
		usage:   "--store=<dir> --key=<name-cert.pub> --policy=<version> [--revocation] [--expires=<rfc3339>] [--staging]",
		summary: "Sign a sitting's batch of daily freshness tickets through ssh-keygen -Y sign with the certified key on its holder's token, into the ticket host's store (§5.4).",
		run:     runComputePolicyTicketsSign,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute policy tickets release",
		usage:   "--store=<dir> --tree=<serving dir> [--staging]",
		summary: "Release each of the newest batch's tickets at its start into the serving tree, deleting every older batch's unreleased tickets and keeping none it published.",
		run:     runComputePolicyTicketsRelease,
	})
}

// `sidecarctl compute policy tickets sign`.
func runComputePolicyTicketsSign(args []string, out io.Writer) error {
	return runComputePolicyTicketsSignWithSettings(args, defaultComputePolicyCommandSettings(), out)
}

// `sidecarctl compute policy tickets release`.
func runComputePolicyTicketsRelease(args []string, out io.Writer) error {
	return runComputePolicyTicketsReleaseWithSettings(args, defaultComputePolicyCommandSettings(), out)
}

// The permissions a store's batch directory takes: the ticket host's alone.
const computePolicyStoreDirectoryMode fs.FileMode = 0o700

// The directory store holds, which must be one.
func computePolicyDirectory(path string, what string) error {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("the %s %s is not a directory", what, path)
	}
	return nil
}

// Every ticket of store, <store>/<batch id>/<yyyy-mm-dd>.bin, that verifies as builder's namespace verifies it, and a
// line for each file that does not. Hidden names are a writer's temporary files and directories, and are skipped.
func readComputePolicyStore(builder *sidecar.ComputePolicyBuilder, store string, settings *computePolicyCommandSettings) ([]*computePolicyTicketFile, []string, error) {
	if err := computePolicyDirectory(store, "ticket store"); err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(store)
	if err != nil {
		return nil, nil, err
	}
	tickets := []*computePolicyTicketFile{}
	refusals := []string{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || !entry.IsDir() {
			continue
		}
		batchTickets, batchRefusals, err := readComputePolicyTicketFiles(builder, filepath.Join(store, entry.Name()), settings)
		if err != nil {
			return nil, nil, err
		}
		tickets = append(tickets, batchTickets...)
		refusals = append(refusals, batchRefusals...)
	}
	return tickets, refusals, nil
}

// Removes path, then syncs its directory, so a deleted ticket stays deleted across a crash.
func removeComputePolicyFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncComputePolicyDirectory(filepath.Dir(path))
}

// Parses tickets sign's command line, and builds, signs, checks and stores the sitting's batch.
func runComputePolicyTicketsSignWithSettings(args []string, settings *computePolicyCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute policy tickets sign", flag.ContinueOnError)
	store := flags.String("store", "", "the ticket host's store, where the batch's directory is written")
	keyPath := flags.String("key", "", "the compute-policy key's OpenSSH certificate, <name>-cert.pub, or the staging key's")
	policyVersion := flags.Uint64("policy", 0, "N, the policy version every ticket requires")
	revocation := flags.Bool("revocation", false, "a revocation's batch, whose first ticket starts at the sitting")
	expiresText := flags.String("expires", "", "the batch's expiry, RFC 3339; 14 days after the sitting by default, and at most")
	staging := flags.Bool("staging", false, "the staging policy's tickets, signed by the staging key")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return sidecarctlUsageErrorf("tickets sign takes no positional argument, got %q", positionals)
	}
	if *store == "" || *keyPath == "" || *policyVersion == 0 {
		return sidecarctlUsageErrorf("name --store, --key and --policy")
	}
	expires, err := parseComputePolicyInstant("expires", *expiresText)
	if err != nil {
		return err
	}
	if err := computePolicyDirectory(*store, "ticket store"); err != nil {
		return err
	}
	builder := newComputePolicyCommandBuilder(settings, *staging)
	batches := sidecar.NewComputeTicketBatchBuilder(&sidecar.ComputeTicketBatchBuilderArgs{
		Settings: settings.build,
		Clock:    settings.clock,
		Random:   settings.random,
	})
	batch, err := batches.Build(&sidecar.ComputeTicketBatchArgs{PolicyVersion: *policyVersion, Expires: expires, Revocation: *revocation})
	if err != nil {
		return err
	}
	line, err := readComputePolicyCertificate(*keyPath, settings)
	if err != nil {
		return err
	}
	certificate, err := builder.Signer(line, batch.Batch.Expires)
	if err != nil {
		return err
	}
	batchDirectory := filepath.Join(*store, batch.Batch.Id.String())
	if _, err := os.Stat(batchDirectory); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s exists, and nothing was written", batchDirectory)
	}
	files, err := batch.Sign(func(message []byte) ([]byte, error) {
		return settings.sign(context.Background(), settings, *keyPath, builder.Namespace(), message)
	})
	if err != nil {
		return err
	}
	for i, file := range files {
		ticket, err := builder.VerifyTicket(file, certificate)
		if err != nil {
			return fmt.Errorf("the ticket from %s does not verify as a device verifies it, and nothing was written: %w",
				batch.Tickets[i].NotBefore.Format(time.RFC3339), err)
		}
		if ticket.Batch.Id != batch.Batch.Id || !ticket.Batch.SignedAt.Equal(batch.Batch.SignedAt) || !ticket.Batch.Expires.Equal(batch.Batch.Expires) ||
			ticket.PolicyVersion != batch.PolicyVersion || !ticket.NotBefore.Equal(batch.Tickets[i].NotBefore) || !ticket.NotAfter.Equal(batch.Tickets[i].NotAfter) {
			return errors.New("a signed ticket is not the one built, and nothing was written")
		}
	}
	// the whole batch into a hidden directory renamed into place, so release never reads half of it
	staged, err := os.MkdirTemp(*store, "."+batch.Batch.Id.String()+".*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	if err := os.Chmod(staged, computePolicyStoreDirectoryMode); err != nil {
		return err
	}
	for i, file := range files {
		path := filepath.Join(staged, sidecar.ComputePolicyTicketDayFileName(batch.Tickets[i].NotBefore))
		if err := createComputePolicyFile(path, file); err != nil {
			return err
		}
	}
	if err := os.Rename(staged, batchDirectory); err != nil {
		return err
	}
	if err := syncComputePolicyDirectory(*store); err != nil {
		return err
	}
	kind := "batch"
	if batch.Revocation {
		kind = "revocation's batch"
	}
	first, last := batch.Tickets[0], batch.Tickets[len(batch.Tickets)-1]
	fmt.Fprintf(out, "signed the %s %s under %s: version %d, signed %s, expires %s, %d tickets from %s to %s, into %s\n", kind, batch.Batch.Id,
		builder.Namespace(), batch.PolicyVersion, batch.Batch.SignedAt.Format(time.RFC3339), batch.Batch.Expires.Format(time.RFC3339), len(files),
		first.NotBefore.Format(time.RFC3339), last.NotBefore.Format(time.RFC3339), batchDirectory)
	return nil
}

// Publishes entry's ticket into tree as tickets/<yyyy-mm-dd>.bin, unless the file there holds a ticket of a newer batch,
// which it never replaces; the same bytes again do nothing.
func publishComputePolicyTicket(builder *sidecar.ComputePolicyBuilder, tree string, entry *computePolicyTicketFile, settings *computePolicyCommandSettings) (string, error) {
	target := filepath.Join(tree, filepath.FromSlash(sidecar.ComputePolicyTicketFileName(entry.ticket.NotBefore)))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	existing, err := os.ReadFile(target)
	switch {
	case err == nil && bytes.Equal(existing, entry.file):
		return target, nil
	case err == nil:
		// a file that does not verify is no ticket a device takes, and is replaced
		if held, verifyErr := builder.VerifyTicket(existing, nil); verifyErr == nil && held.Batch.NewerThan(entry.ticket.Batch) {
			return "", fmt.Errorf("%s holds a ticket of the newer batch %s, which an older one never replaces", target, held.Batch.Id)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	return target, replaceComputePolicyFile(target, entry.file)
}

// Parses tickets release's command line, and releases what is due.
func runComputePolicyTicketsReleaseWithSettings(args []string, settings *computePolicyCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute policy tickets release", flag.ContinueOnError)
	store := flags.String("store", "", "the ticket host's store of unreleased tickets")
	tree := flags.String("tree", "", "the serving tree, which holds <version>.json and tickets/")
	staging := flags.Bool("staging", false, "the staging policy's tickets")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return sidecarctlUsageErrorf("tickets release takes no positional argument, got %q", positionals)
	}
	if *store == "" || *tree == "" {
		return sidecarctlUsageErrorf("name --store and --tree")
	}
	if err := computePolicyDirectory(*tree, "serving tree"); err != nil {
		return err
	}
	builder := newComputePolicyCommandBuilder(settings, *staging)
	now := settings.clock.Now()
	stored, refusals, err := readComputePolicyStore(builder, *store, settings)
	if err != nil {
		return err
	}
	problems := slices.Clone(refusals)

	// the newest batch whose sitting has come: a batch dated ahead is kept, and replaces nothing yet
	var newest *sidecar.ComputePolicyTicketBatch
	ahead := 0
	for _, entry := range stored {
		batch := entry.ticket.Batch
		if now.Before(batch.SignedAt) {
			ahead += 1
			continue
		}
		if newest == nil || batch.NewerThan(*newest) {
			newest = &batch
		}
	}

	// every older batch's tickets are deleted, never only hidden: the store keeps none it published, so each is
	// unreleased, and a revocation's batch leaves none of them to be released
	deletedBatchCounts := map[sidecar.Id]int{}
	current := []*computePolicyTicketFile{}
	for _, entry := range stored {
		batch := entry.ticket.Batch
		switch {
		case now.Before(batch.SignedAt):
		case batch.Id == newest.Id:
			current = append(current, entry)
		default:
			if err := removeComputePolicyFile(entry.path); err != nil {
				problems = append(problems, fmt.Sprintf("%s was not deleted: %s", entry.path, err))
				continue
			}
			deletedBatchCounts[batch.Id] += 1
		}
	}
	deletedBatchIds := slices.SortedFunc(maps.Keys(deletedBatchCounts), func(a sidecar.Id, b sidecar.Id) int {
		return bytes.Compare(a.Bytes(), b.Bytes())
	})
	for _, batchId := range deletedBatchIds {
		fmt.Fprintf(out, "deleted %d unreleased tickets of batch %s, which batch %s supersedes\n", deletedBatchCounts[batchId], batchId, newest.Id)
	}

	// the newest batch's tickets, in order of their starts: each released once started, then deleted from the store
	slices.SortFunc(current, func(a *computePolicyTicketFile, b *computePolicyTicketFile) int {
		return a.ticket.NotBefore.Compare(b.ticket.NotBefore)
	})
	versions, err := computePolicyTreeVersions(*tree)
	if err != nil {
		return err
	}
	servedVersion := uint64(0)
	if 0 < len(versions) {
		servedVersion = versions[0]
	}
	kept := 0
	for _, entry := range current {
		ticket := entry.ticket
		switch {
		case now.Before(ticket.NotBefore):
			kept += 1
			continue
		case !now.Before(ticket.NotAfter) || !now.Before(ticket.Batch.Expires):
			if err := removeComputePolicyFile(entry.path); err != nil {
				problems = append(problems, fmt.Sprintf("%s was not deleted: %s", entry.path, err))
				continue
			}
			fmt.Fprintf(out, "deleted the ticket from %s of batch %s, whose window ended unreleased\n", ticket.NotBefore.Format(time.RFC3339), ticket.Batch.Id)
			continue
		case servedVersion < ticket.PolicyVersion:
			problems = append(problems, fmt.Sprintf("the ticket from %s of batch %s names version %d, which the tree does not serve: publish it first",
				ticket.NotBefore.Format(time.RFC3339), ticket.Batch.Id, ticket.PolicyVersion))
			continue
		}
		target, err := publishComputePolicyTicket(builder, *tree, entry, settings)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if err := removeComputePolicyFile(entry.path); err != nil {
			problems = append(problems, fmt.Sprintf("%s was published and not deleted from the store: %s", entry.path, err))
			continue
		}
		fmt.Fprintf(out, "released the ticket from %s of batch %s, naming version %d, as %s\n", ticket.NotBefore.Format(time.RFC3339), ticket.Batch.Id,
			ticket.PolicyVersion, target)
	}
	if newest != nil {
		fmt.Fprintf(out, "kept %d tickets of batch %s, not yet started\n", kept, newest.Id)
	}
	if 0 < ahead {
		fmt.Fprintf(out, "kept %d tickets of batches whose sitting is still ahead\n", ahead)
	}

	// a batch's directory goes once it holds nothing
	entries, err := os.ReadDir(*store)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || !entry.IsDir() {
			continue
		}
		directory := filepath.Join(*store, entry.Name())
		if files, err := os.ReadDir(directory); err == nil && len(files) == 0 {
			if err := os.Remove(directory); err != nil {
				problems = append(problems, fmt.Sprintf("%s was not removed: %s", directory, err))
			}
		}
	}
	if err := syncComputePolicyDirectory(*store); err != nil {
		return err
	}
	for _, problem := range problems {
		fmt.Fprintf(out, "refused: %s\n", problem)
	}
	if 0 < len(problems) {
		return fmt.Errorf("%d tickets were refused or not handled", len(problems))
	}
	return nil
}
