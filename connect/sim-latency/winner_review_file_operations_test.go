// The broker's readable review context distinguishes authenticated creation
// absence from a real empty base blob before any provider transport is used.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The marker is mandatory exactly for canonical additions. Its empty content
// digest is necessary but not interchangeable with ordinary empty base files.
func TestWinnerReviewModelAuthenticatesAbsentAtBaseMarker(t *testing.T) {
	for _, fixture := range []struct {
		metadata string
		absent   bool
		content  string
		want     bool
	}{
		{metadata: "new file mode 100644\n--- /dev/null\n+++ b/connect/new.txt\n@@ -0,0 +1 @@\n+created\n", absent: true, want: true},
		{metadata: "new file mode 100644\nindex 0000000..e69de29\n", absent: true, want: true},
		{metadata: "new file mode 100644\nindex 0000000..e69de29\n"},
		{metadata: "new file mode 100644\nindex 0000000..e69de29\n", absent: true, content: "invented base\n"},
		{metadata: "--- a/connect/new.txt\n+++ b/connect/new.txt\n@@ -0,0 +1 @@\n+content\n", want: true},
		{metadata: "--- a/connect/new.txt\n+++ b/connect/new.txt\n@@ -0,0 +1 @@\n+content\n", absent: true},
		{metadata: "deleted file mode 100644\nindex e69de29..0000000\n", want: true},
		{metadata: "deleted file mode 100644\nindex e69de29..0000000\n", absent: true},
	} {
		request, _ := winnerModelFixture()
		request.Packet.Patch = []byte("diff --git a/connect/new.txt b/connect/new.txt\n" + fixture.metadata)
		request.Packet.PatchPolicy = controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"connect/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
		request.Packet.SourceCommits = sourceTestCommits("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		request.Packet.BaseSha = request.Packet.SourceCommits["server"]
		lock, err := model.CompetitionSourceLockDigest(request.Packet.SourceCommits)
		if err != nil {
			t.Fatal(err)
		}
		request.Template.BaseSourceLockSha256 = lock
		request.Packet.Entry.BaseSourceLockSha256 = lock
		patchDigest := sha256.Sum256(request.Packet.Patch)
		request.Template.PatchSha256 = hex.EncodeToString(patchDigest[:])
		request.Packet.Entry.PatchSha256 = request.Template.PatchSha256
		digest := sha256.Sum256([]byte(fixture.content))
		request.SourceFiles = []winnerReviewSourceFile{{Path: "connect/new.txt", Content: fixture.content, Sha256: hex.EncodeToString(digest[:]), AbsentAtBase: fixture.absent}}
		input, err := winnerModelInput(request)
		if (err == nil) != fixture.want {
			t.Fatalf("metadata=%q absent=%t content=%q: %v", fixture.metadata, fixture.absent, fixture.content, err)
		}
		if fixture.want && bytes.Contains(input, []byte(`"absent_at_base":true`)) != fixture.absent {
			t.Fatal("readable review input lost the absent/empty distinction")
		}
	}
}
