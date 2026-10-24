// Synthetic build metadata exercises artifact authentication without compiling
// or running candidate-controlled code on the control host.
package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

func sourceBuildInfoFixture() *debug.BuildInfo {
	return &debug.BuildInfo{GoVersion: "go1.27.3", Path: "github.com/urnetwork/server/connect/sim-latency", Main: debug.Module{Path: "github.com/urnetwork/server"},
		Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: strings.Repeat("a", 40)}, {Key: "vcs.modified", Value: "false"},
			{Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "amd64"}, {Key: "CGO_ENABLED", Value: "0"}, {Key: "-buildmode", Value: "exe"},
		}}
}

// Source identity does not authorize a different compiler or an auto-upgrade.
func TestSourceBuildInfoRequiresExactCompetitionGoVersion(t *testing.T) {
	for _, version := range []string{"", "go1.26.6", "go1.27.2", "go1.27.4", "go1.28.0", "devel go1.27.3-synthetic", "go1.27.3-synthetic"} {
		info := sourceBuildInfoFixture()
		info.GoVersion = version
		if err := verifySourceBuildInfo(info, strings.Repeat("a", 40)); err == nil {
			t.Errorf("accepted noncompetition compiler %q", version)
		}
	}
	if err := verifySourceBuildInfo(sourceBuildInfoFixture(), strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
}

func TestSourceBuildInfoRequiresExactCleanHostExecutable(t *testing.T) {
	if err := verifySourceBuildInfo(sourceBuildInfoFixture(), strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*debug.BuildInfo){
		func(info *debug.BuildInfo) { info.Main.Path = "synthetic.example/other" },
		func(info *debug.BuildInfo) { info.Path = "synthetic.example/tool" },
		func(info *debug.BuildInfo) { info.Settings = nil },
		func(info *debug.BuildInfo) { info.Settings = append(info.Settings, info.Settings[0]) },
	} {
		info := sourceBuildInfoFixture()
		mutate(info)
		if verifySourceBuildInfo(info, strings.Repeat("a", 40)) == nil {
			t.Fatal("accepted ambiguous or unrelated program metadata")
		}
	}
	for i := range sourceBuildInfoFixture().Settings {
		info := sourceBuildInfoFixture()
		info.Settings[i].Value = "synthetic-invalid"
		if verifySourceBuildInfo(info, strings.Repeat("a", 40)) == nil {
			t.Fatal("accepted mismatched required setting", info.Settings[i].Key)
		}
	}
	if verifySourceBuildInfo(nil, strings.Repeat("a", 40)) == nil || verifySourceBuildInfo(sourceBuildInfoFixture(), "invalid") == nil {
		t.Fatal("accepted absent metadata or malformed approved commit")
	}
}

func TestSourceBuildBinaryRejectsCompilerTextAndNonExecutables(t *testing.T) {
	for _, content := range [][]byte{nil, []byte("build succeeded"), []byte("synthetic artifact")} {
		if _, err := verifySourceBuildBinary(content, strings.Repeat("a", 40)); err == nil {
			t.Fatal("compiler text was accepted as an executable")
		}
	}
}
