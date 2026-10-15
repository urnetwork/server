package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A real bounded child process supplies narrow Docker output. No Docker
// daemon, container environment, credential or Main contact is involved.
func TestCaptureDockerFixtureProcess(t *testing.T) {
	if os.Getenv("ARIN_DOCKER_FIXTURE") != "1" {
		return
	}
	switch os.Getenv("ARIN_DOCKER_CASE") {
	case "oversize":
		fmt.Print(strings.Repeat("x", (256<<10)+1))
		os.Exit(0)
	case "refuse":
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "--oversize" {
		os.Exit(2)
	}
	// Actual argv is shell-free; the test process flag terminator is supplied
	// by the tiny absolute wrapper below, not by production command expansion.
	if strings.Contains(strings.Join(os.Args, " "), " ps ") {
		fmt.Println(strings.Repeat("a", 64))
		if os.Getenv("ARIN_DOCKER_CASE") == "duplicate" {
			fmt.Println(strings.Repeat("a", 64))
		}
	} else {
		row := hostContainer{ID: strings.Repeat("a", 64), PID: 123, Image: "sha256:" + strings.Repeat("b", 64), StartedAt: time.Now().Add(-time.Hour).UTC(), Environment: "main", Service: "connect", Block: "g1", Version: "v"}
		if os.Getenv("ARIN_DOCKER_CASE") == "missing" {
			os.Exit(0)
		}
		json.NewEncoder(os.Stdout).Encode(row)
	}
	os.Exit(0)
}

func TestCaptureHostDockerBoundsAndHostnameBeforeInspection(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "docker")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Executable path is locally generated; quote explicitly, not as JSON.
	quoted := "'" + strings.ReplaceAll(exe, "'", "'\\''") + "'"
	if os.WriteFile(tool, []byte("#!/bin/sh\nexec "+quoted+" -test.run=^TestCaptureDockerFixtureProcess$ -- \"$@\"\n"), 0700) != nil {
		t.Fatal("fixture write")
	}
	t.Setenv("ARIN_DOCKER_FIXTURE", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := captureHostContainers(ctx, tool, "main")
	if err != nil || len(rows) != 1 || rows[0].PID != 123 {
		t.Fatal("narrow real-process output failed", err)
	}
	for _, which := range []string{"duplicate", "missing", "oversize", "refuse"} {
		t.Setenv("ARIN_DOCKER_CASE", which)
		if _, err = captureHostContainers(ctx, tool, "main"); err == nil {
			t.Fatal("bad bounded output accepted", which)
		}
	}
	marker := filepath.Join(dir, "contacted")
	if os.WriteFile(tool, []byte("#!/bin/sh\nprintf contacted > '"+marker+"'\n"), 0700) != nil {
		t.Fatal("fixture write")
	}
	var out bytes.Buffer
	if runCaptureHostInventory(&out, []string{"--hostname", "definitely-not-this-host", "--env", "main", "--docker", tool, "--output", filepath.Join(dir, "out")}) == nil {
		t.Fatal("wrong host accepted")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) || out.Len() != 0 {
		t.Fatal("privileged metadata preceded host authority")
	}
}

func TestCaptureIdentityRequiresRecordedSourceAndRuntimeBinding(t *testing.T) {
	config, inv := captureTestConfig(t)
	row := inv.Processes[2].Container
	base := inv.Processes[2].Endpoint.Identity
	if !captureIdentityMatches(base, row, "fixture-host", "main", base.Revision, true, time.Now()) {
		t.Fatal("recorded dirty source was treated as forbidden")
	}
	for _, which := range []string{"nonce", "source", "dirty", "image", "host", "block", "version", "role", "before_start", "future"} {
		identity := base
		switch which {
		case "nonce":
			identity.ProcessNonce = server.Id{}
		case "source":
			identity.Revision = strings.Repeat("0", 40)
		case "dirty":
			identity.Modified = false
		case "image":
			identity.ImageDigest = "sha256:" + strings.Repeat("0", 64)
		case "host":
			identity.Host = "other"
		case "block":
			identity.Block = "g2"
		case "version":
			identity.Version = "old"
		case "role":
			identity.Role = "native"
		case "before_start":
			identity.StartedAt = row.StartedAt.Add(-3 * time.Second)
		case "future":
			identity.StartedAt = config.NotAfter.Add(time.Second)
		}
		if captureIdentityMatches(identity, row, "fixture-host", "main", base.Revision, true, time.Now()) {
			t.Fatal("false runtime identity accepted", which)
		}
	}
}
