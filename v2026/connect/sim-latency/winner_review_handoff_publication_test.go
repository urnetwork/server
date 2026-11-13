// A local successful report is published only after protected original custody.
// An uncertain archive response records a failed local report, never acceptance.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/controller"
)

// Encoding with an error must emit a terminal non-authorizing report. The
// native callers own the concrete schema; this helper cannot grant authority.
func publishWinnerHandoffProtectedReport(ctx context.Context, path string, maximum int, minimumUntil time.Time, encode func(error) ([]byte, error), retain func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error)) (*winnerHandoffCustodyOriginal, error) {
	if encode == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("protected report publication lacks its exact path or encoder")
	}
	content, err := encode(nil)
	if err != nil {
		return nil, err
	}
	original, retentionErr := retainWinnerHandoffDocument(ctx, filepath.Base(path), content, maximum, minimumUntil, retain)
	var protected *winnerHandoffCustodyOriginal
	if retentionErr == nil {
		protected = &original
	} else {
		content, err = encode(retentionErr)
		if err != nil {
			return nil, errors.Join(retentionErr, err)
		}
	}
	if len(content) > maximum {
		return protected, errors.Join(retentionErr, errors.New("terminal report exceeds its admitted object bound"))
	}
	writeErr := writePrivateReviewFile(path, content)
	syncErr := syncWinnerHandoffSpoolDirectory(filepath.Dir(path))
	return protected, errors.Join(retentionErr, writeErr, syncErr)
}

func TestWinnerHandoffFailedReportArchiveCannotPublishLocalSuccess(t *testing.T) {
	fixture := newWinnerHandoffSpoolFixture(t)
	fixture.Failure = "upload"
	report := struct {
		Complete  bool   `json:"complete"`
		Qualified bool   `json:"qualified"`
		Error     string `json:"error"`
	}{Complete: true, Qualified: true}
	path := filepath.Join(fixture.Directory, "terminal-report.json")
	original, err := publishWinnerHandoffProtectedReport(t.Context(), path, winnerMultipassSpoolManifestBytes, fixture.Commit.MinimumUntil, func(publicationErr error) ([]byte, error) {
		if publicationErr != nil {
			report.Complete, report.Qualified, report.Error = false, false, publicationErr.Error()
		}
		return json.Marshal(report)
	}, fixture.retain)
	if err == nil || original != nil {
		t.Fatal("failed report archive returned protected success", err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil || json.Unmarshal(content, &report) != nil || report.Complete || report.Qualified || report.Error == "" {
		t.Fatal("archive failure left an authorizing local report", readErr)
	}
}

func TestWinnerHandoffReportPublicationProtectsExactOriginalBeforeLocalSuccess(t *testing.T) {
	fixture := newWinnerHandoffSpoolFixture(t)
	path := filepath.Join(fixture.Directory, "terminal-report.json")
	content := []byte(`{"complete":true,"qualified":false,"mode":"synthetic"}`)
	original, err := publishWinnerHandoffProtectedReport(t.Context(), path, winnerMultipassSpoolManifestBytes, fixture.Commit.MinimumUntil, func(error) ([]byte, error) { return content, nil }, func(ctx context.Context, value []byte, digest string) (*controller.CompetitionWinnerReviewEvidence, error) {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("local report preceded protected custody")
		}
		return fixture.retain(ctx, value, digest)
	})
	if err != nil || original == nil {
		t.Fatal("protected report could not publish", err)
	}
	actual, err := fixture.read(t.Context(), *original)
	if err != nil || string(actual) != string(content) {
		t.Fatal("published report differs from its exact protected original", err)
	}
}
