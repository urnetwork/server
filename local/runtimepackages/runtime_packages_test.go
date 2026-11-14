// Package input contracts keep every service's Docker recipe joined to the
// reviewed byte lock. Tests are offline and need neither services nor Docker.
package runtimepackages

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The manifest records candidate input provenance, not an image approval.
type runtimePackageLock struct {
	Schema            string                `json:"schema"`
	BaseImage         string                `json:"base_image"`
	Snapshot          string                `json:"snapshot"`
	ArchiveSigningKey string                `json:"archive_signing_key"`
	Indexes           []runtimePackageIndex `json:"indexes"`
	Packages          []runtimePackage      `json:"packages"`
	Archives          []runtimeArchive      `json:"archives"`
	Sets              map[string][]string   `json:"sets"`
	Services          map[string]string     `json:"services"`
	ServiceArchives   map[string][]string   `json:"service_archives"`
}

// Package tables are authenticated by the retained signed parent indexes.
type runtimePackageIndex struct {
	Id              string `json:"id"`
	InReleaseSha256 string `json:"inrelease_sha256"`
	PackagesPath    string `json:"packages_path"`
	PackagesSha256  string `json:"packages_sha256"`
}

// Each destination has exactly one reviewed payload and target architecture.
type runtimePackage struct {
	Id           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Architecture string   `json:"architecture"`
	Filename     string   `json:"filename"`
	Sha256       string   `json:"sha256"`
	Size         int64    `json:"size"`
	IndexIds     []string `json:"index_ids"`
}

// A non-Debian payload has its own URL, digest and size, and is unpacked by the
// offline install step on its one target. Only Chrome for Testing headless shell
// zips are admitted, one version for every target.
type runtimeArchive struct {
	Id           string `json:"id"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	Url          string `json:"url"`
	Sha256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Root         string `json:"root"`
	InstallPath  string `json:"install_path"`
}

// Resolve checked-in inputs independently of the command's current directory.
func loadRuntimePackageLock(t *testing.T) (string, runtimePackageLock) {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate runtime package contract")
	}
	cli := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "cli"))
	raw, err := os.ReadFile(filepath.Join(cli, "runtime-packages.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock runtimePackageLock
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatal("trailing package lock data")
	}
	if err := checkRuntimePackageLock(lock); err != nil {
		t.Fatal(err)
	}
	return cli, lock
}

// Validate identities and require both targets for each architecture-specific
// dependency; dropping an arm64 sibling must not silently narrow qualification.
func checkRuntimePackageLock(lock runtimePackageLock) error {
	hexWidth := func(value string, size int) bool {
		raw, err := hex.DecodeString(value)
		return err == nil && len(raw) == size && strings.ToLower(value) == value && strings.Trim(value, "0") != ""
	}
	base, hash, found := strings.Cut(lock.BaseImage, "@sha256:")
	if lock.Schema != "urnetwork-server-runtime-packages-v1" || !found || base != "ubuntu:24.04" || !hexWidth(hash, 32) ||
		!strings.HasPrefix(lock.Snapshot, "https://") || !strings.HasSuffix(lock.Snapshot, "/") || len(lock.ArchiveSigningKey) != 40 {
		return errors.New("invalid package lock identity")
	}
	indexes := map[string]runtimePackageIndex{}
	for _, index := range lock.Indexes {
		_, duplicate := indexes[index.Id]
		parts := strings.Split(index.Id, "/")
		if duplicate || len(parts) != 2 || !slices.Contains([]string{"amd64", "arm64"}, parts[1]) ||
			index.PackagesPath != "main/binary-"+parts[1]+"/Packages.xz" || !hexWidth(index.InReleaseSha256, 32) || !hexWidth(index.PackagesSha256, 32) {
			return errors.New("invalid signed index reference")
		}
		indexes[index.Id] = index
	}
	packages := map[string]runtimePackage{}
	for _, pkg := range lock.Packages {
		_, duplicate := packages[pkg.Id]
		if duplicate || pkg.Id != pkg.Name+"_"+pkg.Architecture || strings.ContainsAny(pkg.Name, " /\\\t\n$*") || pkg.Name == "" || pkg.Version == "" ||
			!slices.Contains([]string{"all", "amd64", "arm64"}, pkg.Architecture) || !hexWidth(pkg.Sha256, 32) || pkg.Size <= 0 || pkg.Size > 8*1024*1024 ||
			path.Clean(pkg.Filename) != pkg.Filename || !strings.HasPrefix(pkg.Filename, "pool/") || strings.ContainsAny(pkg.Filename, " \t\n$?#") || len(pkg.IndexIds) == 0 {
			return fmt.Errorf("invalid package identity %q", pkg.Id)
		}
		architectures := map[string]bool{}
		for _, id := range pkg.IndexIds {
			index, exists := indexes[id]
			if !exists {
				return fmt.Errorf("unknown index %s", id)
			}
			arch := strings.Split(index.Id, "/")[1]
			if pkg.Architecture != "all" && arch != pkg.Architecture {
				return errors.New("package/index architecture mismatch")
			}
			architectures[arch] = true
		}
		if pkg.Architecture == "all" && len(architectures) != 2 {
			return errors.New("shared package is absent from one target's signed index")
		}
		packages[pkg.Id] = pkg
	}
	archives := map[string]runtimeArchive{}
	platforms := map[string]string{"amd64": "linux64", "arm64": "linux-arm64"}
	for _, archive := range lock.Archives {
		_, duplicate := archives[archive.Id]
		platform := platforms[archive.Architecture]
		parts := strings.Split(archive.Version, ".")
		numeric := len(parts) == 4
		for _, part := range parts {
			numeric = numeric && part != "" && strings.Trim(part, "0123456789") == ""
		}
		if duplicate || !numeric || platform == "" || archive.Version != lock.Archives[0].Version || archive.Type != "zip" || archive.Name != "chrome-headless-shell" ||
			archive.Id != archive.Name+"_"+archive.Architecture || !hexWidth(archive.Sha256, 32) || archive.Size <= 0 || archive.Size > 512*1024*1024 ||
			archive.Url != "https://storage.googleapis.com/chrome-for-testing-public/"+archive.Version+"/"+platform+"/"+archive.Name+"-"+platform+".zip" ||
			archive.Root != archive.Name+"-"+platform || archive.InstallPath != "/opt/"+archive.Name {
			return fmt.Errorf("invalid archive identity %q", archive.Id)
		}
		archives[archive.Id] = archive
	}
	for name, ids := range lock.Sets {
		if name == "" || len(ids) == 0 {
			return errors.New("empty package set")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			pkg, exists := packages[id]
			if !exists || seen[id] {
				return errors.New("missing or duplicate package in set")
			}
			seen[id] = true
			if pkg.Architecture != "all" {
				peerArch := "arm64"
				if pkg.Architecture == "arm64" {
					peerArch = "amd64"
				}
				peer, exists := packages[pkg.Name+"_"+peerArch]
				if !exists || peer.Version != pkg.Version || !slices.Contains(ids, peer.Id) {
					return fmt.Errorf("incomplete target pair for %s", pkg.Name)
				}
			}
		}
	}
	wantServices := map[string]string{"alt": "certificates", "api": "certificates", "connect": "certificates", "gossip": "certificates", "mcp": "certificates", "taskworker": "taskworker", "proxy": "proxy"}
	if !reflect.DeepEqual(lock.Services, wantServices) {
		return errors.New("service package coverage changed")
	}
	wantCertificates := []string{"ca-certificates_all", "libssl3t64_amd64", "libssl3t64_arm64", "openssl_amd64", "openssl_arm64"}
	if !slices.Equal(lock.Sets["certificates"], wantCertificates) || len(lock.Sets) != 3 {
		return errors.New("certificate installation lost its complete target inputs")
	}
	if !slices.Contains(lock.Sets["proxy"], "curl_amd64") || !slices.Contains(lock.Sets["proxy"], "curl_arm64") {
		return errors.New("proxy lost curl")
	}
	for name, ids := range lock.Sets {
		for _, id := range lock.Sets["certificates"] {
			if !slices.Contains(ids, id) {
				return fmt.Errorf("%s lost certificate dependency", name)
			}
		}
	}
	// The headless shell's NEEDED libraries, the font configuration that gives
	// Blink a last-resort font while web fonts load, and the offline extractor.
	for _, name := range []string{"fontconfig-config", "libasound2t64", "libatk-bridge2.0-0t64", "libatk1.0-0t64", "libatspi2.0-0t64", "libdbus-1-3", "libexpat1", "libgbm1", "libglib2.0-0t64",
		"libnspr4", "libnss3", "libx11-6", "libxcb1", "libxcomposite1", "libxdamage1", "libxext6", "libxfixes3", "libxkbcommon0", "libxrandr2", "unzip"} {
		if !slices.Contains(lock.Sets["taskworker"], name+"_amd64") || !slices.Contains(lock.Sets["taskworker"], name+"_arm64") {
			return fmt.Errorf("taskworker lost %s", name)
		}
	}
	wantServiceArchives := map[string][]string{"taskworker": {"chrome-headless-shell_amd64", "chrome-headless-shell_arm64"}}
	if !reflect.DeepEqual(lock.ServiceArchives, wantServiceArchives) {
		return errors.New("service archive coverage changed")
	}
	used := map[string]bool{}
	for role, ids := range lock.ServiceArchives {
		for _, id := range ids {
			archive, exists := archives[id]
			if !exists || !slices.Contains(lock.Sets[lock.Services[role]], "unzip_"+archive.Architecture) {
				return fmt.Errorf("%s archive %s is unknown or cannot be unpacked offline", role, id)
			}
			used[id] = true
		}
	}
	if len(used) != len(archives) {
		return errors.New("unused archive input")
	}
	return nil
}

// The taskworker's gift card printer runs Chromium as this unprivileged user
// (controller/gift_cards_pdf.go): fixed passwd, group, shadow and gshadow lines
// written by the offline install step, so the image stays reproducible.
const taskworkerPrinterUser = " && echo 'giftcard:x:10001:10001:gift card printer:/nonexistent:/usr/sbin/nologin' >> /etc/passwd" +
	" && echo 'giftcard:x:10001:' >> /etc/group" +
	" && echo 'giftcard:!:::::::' >> /etc/shadow" +
	" && echo 'giftcard:!::' >> /etc/gshadow"

// Check the executable instructions rather than matching comments or whitespace.
// This intentionally covers the simple service recipes, not arbitrary Dockerfiles.
func checkRuntimePackageRecipe(lock runtimePackageLock, role, recipe string) error {
	wanted := map[string]bool{}
	for _, id := range lock.Sets[lock.Services[role]] {
		for _, pkg := range lock.Packages {
			if pkg.Id == id {
				wanted["ADD --checksum=sha256:"+pkg.Sha256+" "+lock.Snapshot+pkg.Filename+" /runtime-packages/"+pkg.Architecture+"/"+pkg.Name+".deb"] = true
			}
		}
	}
	// An archive is fetched beside the packages and unpacked by the same offline
	// step, after dpkg installs unzip, only on its own target.
	install := "RUN --network=none --mount=from=runtime-packages,source=/runtime-packages,target=/runtime-packages,ro dpkg -i /runtime-packages/\"$(dpkg --print-architecture)\"/*.deb /runtime-packages/all/*.deb && update-ca-certificates"
	for _, id := range lock.ServiceArchives[role] {
		for _, archive := range lock.Archives {
			if archive.Id == id {
				payload := "/runtime-packages/" + archive.Architecture + "/" + archive.Name + ".zip"
				wanted["ADD --checksum=sha256:"+archive.Sha256+" "+archive.Url+" "+payload] = true
				parent := path.Dir(archive.InstallPath)
				install += " && if [ \"$(dpkg --print-architecture)\" = " + archive.Architecture + " ]; then unzip -q " + payload + " -d " + parent + " && mv " + path.Join(parent, archive.Root) + " " + archive.InstallPath + "; fi"
			}
		}
	}
	if role == "taskworker" {
		install += taskworkerPrinterUser
	}
	install += " && rm -f /var/cache/ldconfig/aux-cache /var/log/dpkg.log"
	stage, installs := 0, 0
	pending := ""
	for _, line := range strings.Split(recipe, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			pending += strings.TrimSpace(strings.TrimSuffix(line, "\\")) + " "
			continue
		}
		instruction := pending + line
		pending = ""
		switch {
		case strings.HasPrefix(instruction, "FROM "):
			stage++
			want := "FROM " + lock.BaseImage
			if stage == 1 {
				want += " AS runtime-packages"
			}
			if stage > 2 || instruction != want || stage == 2 && len(wanted) != 0 {
				return errors.New("unlocked or incomplete package base stage")
			}
		case strings.HasPrefix(instruction, "ADD "):
			if stage != 1 || !wanted[instruction] {
				return errors.New("unlocked, changed or repeated remote package input")
			}
			delete(wanted, instruction)
		case strings.HasPrefix(instruction, "RUN "):
			if stage != 2 {
				return errors.New("package download stage gained execution")
			}
			if instruction == "RUN ln -s /srv/warp/vault/.aws /root/.aws" {
				continue
			}
			if instruction != install {
				return errors.New("package installation gained an unreviewed command, resolver or network")
			}
			installs++
		default:
			if stage != 2 {
				return errors.New("unreviewed package stage instruction")
			}
			verb, _, _ := strings.Cut(instruction, " ")
			if !slices.Contains([]string{"ARG", "ENV", "COPY", "STOPSIGNAL", "CMD"}, verb) || strings.Contains(instruction, "COPY --from=") {
				return errors.New("unreviewed image input or deferred execution")
			}
		}
	}
	if pending != "" || stage != 2 || installs != 1 || len(wanted) != 0 {
		return errors.New("incomplete package install")
	}
	return nil
}

// Every adjacent service consumes the same checked-in package authority.
func TestRuntimePackagesJoinAllServiceRecipes(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	paths, err := filepath.Glob(filepath.Join(cli, "*", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, filename := range paths {
		role := filepath.Base(filepath.Dir(filename))
		raw, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if role == "competitionworker" && strings.HasPrefix(string(raw), "FROM scratch\n") {
			continue
		}
		if _, exists := lock.Services[role]; !exists {
			t.Fatalf("unreviewed service Dockerfile %s", role)
		}
		if err := checkRuntimePackageRecipe(lock, role, string(raw)); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		seen[role] = true
	}
	if len(seen) != len(lock.Services) {
		t.Fatal("missing service recipe")
	}
}

// The original moving-repository recipe and adjacent input/boundary mutations
// must fail even when the base image remains pinned.
func TestRuntimePackagesRejectMovingAndIncompleteInputs(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	raw, err := os.ReadFile(filepath.Join(cli, "proxy", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	good := string(raw)
	if err := checkRuntimePackageRecipe(lock, "proxy", good); err != nil {
		t.Fatal(err)
	}
	start := strings.Index(good, "ADD ")
	end := start + strings.IndexByte(good[start:], '\n') + 1
	for _, fault := range []string{"moving-repository", "missing-payload", "wrong-hash", "override", "online-install", "tag-only", "duplicate-payload", "deferred-resolver"} {
		changed := good
		switch fault {
		case "moving-repository":
			changed = strings.Replace(changed, "RUN --network=none --mount=from=runtime-packages,source=/runtime-packages,target=/runtime-packages,ro \\", "RUN apt-get update && apt-get install -y curl && \\", 1)
		case "missing-payload":
			changed = changed[:start] + changed[end:]
		case "wrong-hash":
			changed = strings.Replace(changed, "--checksum=sha256:", "--checksum=sha256:0", 1)
		case "override":
			changed = strings.Replace(changed, lock.Snapshot, "${PACKAGE_MIRROR}/", 1)
		case "online-install":
			changed = strings.Replace(changed, "--network=none", "--network=default", 1)
		case "tag-only":
			changed = strings.ReplaceAll(changed, lock.BaseImage, "ubuntu:24.04")
		case "duplicate-payload":
			changed = changed[:end] + changed[start:end] + changed[end:]
		case "deferred-resolver":
			changed += "\nONBUILD RUN apt-get update && apt-get install -y curl\n"
		}
		if err := checkRuntimePackageRecipe(lock, "proxy", changed); err == nil {
			t.Fatalf("%s escaped package admission", fault)
		}
	}
}

// Architecture and role omissions are rejected independently of recipe text.
func TestRuntimePackagesRejectCrossTargetAndCurlGaps(t *testing.T) {
	_, lock := loadRuntimePackageLock(t)
	for _, fault := range []string{"arm64-pair", "curl", "duplicate", "index-architecture"} {
		raw, _ := json.Marshal(lock)
		var changed runtimePackageLock
		if err := json.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "arm64-pair":
			changed.Sets["certificates"] = slices.DeleteFunc(changed.Sets["certificates"], func(id string) bool { return id == "openssl_arm64" })
		case "curl":
			changed.Sets["proxy"] = slices.DeleteFunc(changed.Sets["proxy"], func(id string) bool { return strings.HasPrefix(id, "curl_") })
		case "duplicate":
			changed.Packages = append(changed.Packages, changed.Packages[0])
		case "index-architecture":
			for i, pkg := range changed.Packages {
				if pkg.Architecture == "arm64" {
					changed.Packages[i].IndexIds = []string{"noble-updates/amd64"}
					break
				}
			}
		}
		if err := checkRuntimePackageLock(changed); err == nil {
			t.Fatalf("%s lost package coverage", fault)
		}
	}
}

// Each headless shell archive keeps the package boundary: one literal URL and
// digest, fetched only by the download stage, unpacked only offline on its own
// target.
func TestRuntimePackagesRejectUnpinnedChromiumArchive(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	raw, err := os.ReadFile(filepath.Join(cli, "taskworker", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	good := string(raw)
	if err := checkRuntimePackageRecipe(lock, "taskworker", good); err != nil {
		t.Fatal(err)
	}
	archive, armArchive := lock.Archives[0], lock.Archives[1]
	add := "ADD --checksum=sha256:" + archive.Sha256 + " " + archive.Url + " /runtime-packages/amd64/chrome-headless-shell.zip\n"
	armAdd := "ADD --checksum=sha256:" + armArchive.Sha256 + " " + armArchive.Url + " /runtime-packages/arm64/chrome-headless-shell.zip\n"
	extract := "unzip -q /runtime-packages/amd64/chrome-headless-shell.zip -d /opt"
	for _, fault := range []string{"wrong-checksum", "missing-checksum", "missing-archive", "duplicate-archive", "moved-url", "unpinned-url",
		"online-extraction", "install-time-fetch", "download-stage-extraction", "final-stage-download", "copied-archive", "every-target",
		"arm64-wrong-checksum", "arm64-missing-archive", "cross-target-archive"} {
		changed := good
		switch fault {
		case "wrong-checksum":
			changed = strings.Replace(changed, archive.Sha256, strings.Repeat("0", 63)+"1", 1)
		case "arm64-wrong-checksum":
			changed = strings.Replace(changed, armArchive.Sha256, strings.Repeat("0", 63)+"1", 1)
		case "arm64-missing-archive":
			changed = strings.Replace(changed, armAdd, "", 1)
		case "cross-target-archive":
			changed = strings.Replace(changed, armAdd, strings.Replace(add, "/runtime-packages/amd64/", "/runtime-packages/arm64/", 1), 1)
		case "missing-checksum":
			changed = strings.Replace(changed, add, "ADD "+archive.Url+" /runtime-packages/amd64/chrome-headless-shell.zip\n", 1)
		case "missing-archive":
			changed = strings.Replace(changed, add, "", 1)
		case "duplicate-archive":
			changed = strings.Replace(changed, add, add+add, 1)
		case "moved-url":
			changed = strings.Replace(changed, "https://storage.googleapis.com/chrome-for-testing-public/", "https://cdn.playwright.dev/chrome-for-testing-public/", 1)
		case "unpinned-url":
			changed = strings.Replace(changed, archive.Url, "${CHROME_HEADLESS_SHELL_URL}", 1)
		case "online-extraction":
			changed = strings.Replace(changed, "RUN --network=none --mount=", "RUN --network=default --mount=", 1)
		case "install-time-fetch":
			changed = strings.Replace(changed, extract, "curl -fsSL "+archive.Url+" -o /tmp/shell.zip && unzip -q /tmp/shell.zip -d /opt", 1)
		case "download-stage-extraction":
			changed = strings.Replace(changed, add, add+"RUN unzip -q /runtime-packages/amd64/chrome-headless-shell.zip -d /runtime-packages/amd64\n", 1)
		case "final-stage-download":
			changed = strings.Replace(strings.Replace(changed, add, "", 1), "COPY build/", add+"COPY build/", 1)
		case "copied-archive":
			changed = strings.Replace(changed, "COPY build/", "COPY --from=runtime-packages /runtime-packages/amd64/chrome-headless-shell.zip /opt/\nCOPY build/", 1)
		case "every-target":
			changed = strings.Replace(strings.Replace(changed, "if [ \"$(dpkg --print-architecture)\" = amd64 ]; then ", "", 1), "; fi \\", " \\", 1)
		}
		if changed == good || checkRuntimePackageRecipe(lock, "taskworker", changed) == nil {
			t.Fatalf("%s escaped archive admission", fault)
		}
	}
}

// The printer's user stays unprivileged and literal: not root, not a login,
// not the image's other users, and not a resolver's choice at build time.
func TestRuntimePackagesTaskworkerPrinterUser(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	raw, err := os.ReadFile(filepath.Join(cli, "taskworker", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	good := string(raw)
	if err := checkRuntimePackageRecipe(lock, "taskworker", good); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"root", "root-group", "login-shell", "password", "useradd", "missing"} {
		changed := good
		switch fault {
		case "root":
			changed = strings.Replace(changed, "giftcard:x:10001:10001:", "giftcard:x:0:10001:", 1)
		case "root-group":
			changed = strings.Replace(changed, "giftcard:x:10001:10001:", "giftcard:x:10001:0:", 1)
		case "login-shell":
			changed = strings.Replace(changed, ":/nonexistent:/usr/sbin/nologin", ":/home/giftcard:/bin/bash", 1)
		case "password":
			changed = strings.Replace(changed, "'giftcard:!:::::::'", "'giftcard::::::::'", 1)
		case "useradd":
			changed = strings.Replace(changed, "echo 'giftcard:x:10001:10001:gift card printer:/nonexistent:/usr/sbin/nologin' >> /etc/passwd", "useradd --system giftcard", 1)
		case "missing":
			for _, line := range []string{
				"    && echo 'giftcard:x:10001:10001:gift card printer:/nonexistent:/usr/sbin/nologin' >> /etc/passwd \\\n",
				"    && echo 'giftcard:x:10001:' >> /etc/group \\\n",
				"    && echo 'giftcard:!:::::::' >> /etc/shadow \\\n",
				"    && echo 'giftcard:!::' >> /etc/gshadow \\\n",
			} {
				changed = strings.Replace(changed, line, "", 1)
			}
		}
		if changed == good || checkRuntimePackageRecipe(lock, "taskworker", changed) == nil {
			t.Fatalf("%s escaped the printer user's admission", fault)
		}
	}
	// the other services add no user
	raw, err = os.ReadFile(filepath.Join(cli, "api", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "giftcard") {
		t.Fatal("the api image has the printer's user")
	}
}

// Archive identity, coverage and the headless shell's runtime packages are
// lock properties, independent of recipe text.
func TestRuntimePackagesRejectArchiveAndChromiumGaps(t *testing.T) {
	_, lock := loadRuntimePackageLock(t)
	for _, fault := range []string{"version-url", "split-version", "mirror", "checksum", "size", "target", "cross-root", "install-path", "coverage", "arm64-coverage", "unused",
		"unzip", "library", "font-config"} {
		raw, _ := json.Marshal(lock)
		var changed runtimePackageLock
		if err := json.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "version-url":
			changed.Archives[0].Version = "150.0.7871.0"
		case "split-version":
			changed.Archives[1].Url = strings.Replace(changed.Archives[1].Url, changed.Archives[1].Version, "156.0.8000.1", 1)
			changed.Archives[1].Version = "156.0.8000.1"
		case "mirror":
			changed.Archives[0].Url = strings.Replace(changed.Archives[0].Url, "storage.googleapis.com", "cdn.playwright.dev", 1)
		case "checksum":
			changed.Archives[0].Sha256 = ""
		case "size":
			changed.Archives[0].Size = 0
		case "target":
			changed.Archives[1].Architecture, changed.Archives[1].Id = "armhf", "chrome-headless-shell_armhf"
			changed.ServiceArchives["taskworker"] = []string{"chrome-headless-shell_amd64", "chrome-headless-shell_armhf"}
		case "cross-root":
			changed.Archives[1].Root = changed.Archives[0].Root
		case "arm64-coverage":
			changed.ServiceArchives["taskworker"] = []string{"chrome-headless-shell_amd64"}
		case "install-path":
			changed.Archives[0].InstallPath = "/usr/local/bin"
		case "coverage":
			changed.ServiceArchives["api"] = []string{"chrome-headless-shell_amd64"}
		case "unused":
			changed.ServiceArchives = map[string][]string{}
		case "unzip", "library", "font-config":
			prefix := map[string]string{"unzip": "unzip_", "library": "libnss3_", "font-config": "fontconfig-config_"}[fault]
			changed.Sets["taskworker"] = slices.DeleteFunc(changed.Sets["taskworker"], func(id string) bool { return strings.HasPrefix(id, prefix) })
		}
		if err := checkRuntimePackageLock(changed); err == nil {
			t.Fatalf("%s escaped archive or Chromium admission", fault)
		}
	}
}
