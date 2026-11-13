// The command verifies existing artifacts read-only and prints finite metadata.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
)

// Zero exit status is reserved for a completely verified usable image. Expected
// negative controls retain failed=false semantics and must check exit one/code.
func main() {
	settings := defaultVerifierSettings()
	native := flag.String("native", "", "existing native payload path")
	archive := flag.String("archive", "", "existing Docker save archive path")
	wanted := archiveExpectation{}
	flag.StringVar(&wanted.NativeSha256, "native-sha256", "", "reviewed native SHA-256")
	flag.StringVar(&wanted.ArchiveSha256, "archive-sha256", "", "reviewed archive SHA-256")
	flag.StringVar(&wanted.ImageId, "image-id", "", "reviewed image configuration identity")
	flag.StringVar(&wanted.RepoTag, "tag", "", "exact source-owned image tag")
	flag.StringVar(&wanted.Revision, "revision", "", "exact clean source revision")
	flag.StringVar(&wanted.User, "user", "65532:65532", "expected numeric nonroot uid/gid")
	flag.Parse()
	if flag.NArg() != 0 {
		os.Exit(2)
	}
	report, err := verifyArchive(context.Background(), *native, *archive, wanted, settings)
	if json.NewEncoder(os.Stdout).Encode(report) != nil || err != nil || !report.Passed {
		os.Exit(1)
	}
}
