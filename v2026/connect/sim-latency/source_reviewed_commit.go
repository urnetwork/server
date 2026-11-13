// Re-materializing one approved patch must reproduce the same Git identity,
// not create a different source lock when a build controller restarts later.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Git author/committer timestamps come from authenticated review authority.
// Ambient identities and clocks must not select a new image or test draw.
func commitReviewedSource(ctx context.Context, directory string, reviewId server.Id, reviewedAt time.Time) (string, error) {
	if reviewId == (server.Id{}) || reviewedAt.IsZero() {
		return "", errors.New("reviewed source commit requires its authenticated review identity and timestamp")
	}
	command, err := sourceGitLocalCommand(ctx, directory, "-c", "core.fsync=all", "-c", "i18n.commitEncoding=UTF-8",
		"commit", "--quiet", "--no-gpg-sign", "--cleanup=verbatim", "-m", "competition: separately reviewed source "+reviewId.String())
	if err != nil {
		return "", err
	}
	filtered := command.Env[:0]
	for _, entry := range command.Env {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_AUTHOR_") || strings.HasPrefix(name, "GIT_COMMITTER_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	date := fmt.Sprintf("@%d +0000", reviewedAt.Unix())
	command.Env = append(filtered,
		"GIT_AUTHOR_NAME=URnetwork Competition", "GIT_AUTHOR_EMAIL=competition@invalid", "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME=URnetwork Competition", "GIT_COMMITTER_EMAIL=competition@invalid", "GIT_COMMITTER_DATE="+date)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("deterministic reviewed source commit failed: %w", err)
	}
	return sourceGitText(ctx, directory, "rev-parse", "HEAD")
}
