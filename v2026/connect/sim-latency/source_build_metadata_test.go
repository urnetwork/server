// Synthetic archives force path traversal and link attacks without trusting
// host tar extraction or requiring a Docker daemon.
package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func sourceBuildMetadataArchive(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && header.Size > 0 {
			if _, err := writer.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestSourceBuildMetadataRejectsArchiveEscapesLinksAndDuplicates(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg}, {Name: "/escape", Typeflag: tar.TypeReg},
		{Name: "nested/file", Typeflag: tar.TypeReg}, {Name: "..", Typeflag: tar.TypeReg},
		{Name: "cache\\escape", Typeflag: tar.TypeReg}, {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../escape"},
		{Name: "hard", Typeflag: tar.TypeLink, Linkname: "/escape"}, {Name: "device", Typeflag: tar.TypeChar},
	} {
		if _, err := decodeSourceBuildMetadata(sourceBuildMetadataArchive(t, []*tar.Header{header}), defaultSourceBuildSandboxSettings()); err == nil {
			t.Fatal("unsafe metadata entry accepted", header.Name, header.Typeflag)
		}
	}
	duplicate := &tar.Header{Name: "version.mod", Typeflag: tar.TypeReg}
	if _, err := decodeSourceBuildMetadata(sourceBuildMetadataArchive(t, []*tar.Header{duplicate, duplicate}), defaultSourceBuildSandboxSettings()); err == nil {
		t.Fatal("duplicate metadata entry accepted")
	}
}

func TestSourceBuildMetadataBudgetsAndIndependentSeeds(t *testing.T) {
	archive := sourceBuildMetadataArchive(t, []*tar.Header{{Name: "./", Typeflag: tar.TypeDir}, {Name: "./version.mod", Typeflag: tar.TypeReg, Size: 4}})
	settings := defaultSourceBuildSandboxSettings()
	entries, err := decodeSourceBuildMetadata(archive, settings)
	if err != nil || string(entries["version.mod"]) != "xxxx" {
		t.Fatal("valid flat cache archive failed", err)
	}
	root := t.TempDir()
	first, second := filepath.Join(root, "check"), filepath.Join(root, "compile")
	t.Cleanup(func() {
		// Only test-owned seed directories are made removable after assertions.
		for _, directory := range []string{first, second} {
			if err := os.Chmod(directory, 0o700); err != nil && !os.IsNotExist(err) {
				t.Error(err)
			}
		}
	})
	if err := writeSourceBuildMetadata(first, entries); err != nil {
		t.Fatal(err)
	}
	if err := writeSourceBuildMetadata(second, entries); err != nil {
		t.Fatal(err)
	}
	firstFile, secondFile := filepath.Join(first, "version.mod"), filepath.Join(second, "version.mod")
	firstInfo, err := os.Stat(firstFile)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(secondFile)
	if err != nil || os.SameFile(firstInfo, secondInfo) || firstInfo.Mode().Perm() != 0o444 || secondInfo.Mode().Perm() != 0o444 {
		t.Fatal("metadata seeds share writable objects or incorrect permissions", err)
	}
	if err := writeSourceBuildMetadata(first, entries); err == nil {
		t.Fatal("seed retry overwrote existing phase evidence")
	}
	settings.MetadataBytes = int64(len(archive) - 1)
	if _, err := decodeSourceBuildMetadata(archive, settings); err == nil {
		t.Fatal("oversized metadata archive accepted")
	}
	settings = defaultSourceBuildSandboxSettings()
	settings.MetadataFiles = 1
	if _, err := decodeSourceBuildMetadata(sourceBuildMetadataArchive(t, []*tar.Header{{Name: "first", Typeflag: tar.TypeReg}, {Name: "second", Typeflag: tar.TypeReg}}), settings); err == nil {
		t.Fatal("metadata entry count exceeded its budget")
	}
	if _, err := decodeSourceBuildMetadata(sourceBuildMetadataArchive(t, nil), settings); err == nil {
		t.Fatal("empty archive accepted")
	}
}

func TestSourceBuildMetadataExportCannotShadowIncumbentCache(t *testing.T) {
	input := sourceSandboxFixture()
	input.Phase = "metadata"
	input.MetadataDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), input.Phase, "metadata")
	input.OutputDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), input.Phase, "output")
	args, err := sourceBuildSandboxArguments(input, sourceSandboxTestSettings())
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if bytes.HasPrefix([]byte(arg), []byte("--tmpfs=/opt/urnetwork/go/pkg/mod/cache/download/")) {
			t.Fatal("metadata export shadowed the immutable cache it needs to read")
		}
	}
	if args[len(args)-1] != "tar -C /opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v -cf - ." {
		t.Fatal("metadata export invoked candidate code or a writable build phase")
	}
}
