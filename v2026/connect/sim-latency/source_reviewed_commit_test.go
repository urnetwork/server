// Independent checkouts and conflicting ambient dates deterministically expose
// source-lock drift without relying on sleeps or crossing a wall-clock second.
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestReviewedSourceCommitReproducesAcrossCheckoutsAndAmbientIdentities(t *testing.T) {
	root := t.TempDir()
	base := sourceTestRepository(t, root, "server")
	reviewId := server.NewId()
	reviewedAt := time.Unix(1700000000, 123456789).UTC()
	var first string
	for index, date := range []string{"@1000000000 +0000", "@1900000000 -0700"} {
		t.Setenv("GIT_AUTHOR_NAME", "Synthetic Ambient Author "+date)
		t.Setenv("GIT_AUTHOR_EMAIL", "ambient-author@example.invalid")
		t.Setenv("GIT_AUTHOR_DATE", date)
		t.Setenv("GIT_COMMITTER_NAME", "Synthetic Ambient Committer "+date)
		t.Setenv("GIT_COMMITTER_EMAIL", "ambient-committer@example.invalid")
		t.Setenv("GIT_COMMITTER_DATE", date)
		checkout := filepath.Join(root, []string{"first", "replacement"}[index])
		if _, err := sourceGitText(context.Background(), root, "clone", "--quiet", "--no-checkout", filepath.Join(root, "server"), checkout); err != nil {
			t.Fatal(err)
		}
		if _, err := sourceGitText(context.Background(), checkout, "checkout", "--quiet", "--detach", base); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(checkout, "synthetic-approved.txt"), []byte("same reviewed source tree\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := sourceGitText(context.Background(), checkout, "add", "synthetic-approved.txt"); err != nil {
			t.Fatal(err)
		}
		commit, err := commitReviewedSource(context.Background(), checkout, reviewId, reviewedAt)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = commit
		} else if commit != first {
			t.Fatal("restarting the same review changed its source commit", first, commit)
		}
		identity := sourceTestGit(t, checkout, "show", "-s", "--format=%an|%ae|%at|%cn|%ce|%ct|%P", commit)
		want := "URnetwork Competition|competition@invalid|1700000000|URnetwork Competition|competition@invalid|1700000000|" + base
		if strings.TrimSpace(identity) != want {
			t.Fatal("ambient clock or identity leaked into reviewed source", identity)
		}
	}
	if got := sourceTestGit(t, filepath.Join(root, "server"), "rev-parse", "HEAD"); got != base {
		t.Fatal("review commit moved the original repository")
	}
}

func TestReviewedSourceCommitRequiresAuthenticatedReviewIdentityAndTime(t *testing.T) {
	root := t.TempDir()
	base := sourceTestRepository(t, root, "server")
	for _, missingTime := range []bool{false, true} {
		reviewId, reviewedAt := server.NewId(), time.Unix(1700000000, 0).UTC()
		if missingTime {
			reviewedAt = time.Time{}
		} else {
			reviewId = server.Id{}
		}
		if commit, err := commitReviewedSource(context.Background(), filepath.Join(root, "server"), reviewId, reviewedAt); err == nil || commit != "" {
			t.Fatal("missing authority produced a source commit", err)
		}
	}
	if got := sourceTestGit(t, filepath.Join(root, "server"), "rev-parse", "HEAD"); got != base {
		t.Fatal("invalid review mutated source")
	}
}
