// Synthetic ownership observations exercise protected-path policy without
// claiming root execution or relying on the test user's actual uid.
package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Keep real file kind/canonicalization and replace only ownership and modes.
type sourceHostProbeFileInfo struct {
	os.FileInfo
	Owner       uint32
	Permissions os.FileMode
}

func (self sourceHostProbeFileInfo) Sys() any {
	return &syscall.Stat_t{Uid: self.Owner}
}

func (self sourceHostProbeFileInfo) Mode() os.FileMode {
	return self.FileInfo.Mode()&os.ModeType | self.Permissions
}

// Real symlink/canonicalization checks stay enabled; only stat ownership and
// permission values are synthetic, like the companion shell-helper tests.
func TestSourceHostTrustedPathChecksEveryAncestor(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "host.json")
	if err := os.WriteFile(path, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", path, parent, filepath.Dir(parent), "/"} {
		for _, wrongOwner := range []bool{false, true} {
			stat := func(current string) (os.FileInfo, error) {
				info, err := os.Lstat(current)
				if err != nil {
					return nil, err
				}
				observed := sourceHostProbeFileInfo{FileInfo: info, Owner: 0, Permissions: 0o755}
				if !info.IsDir() {
					observed.Permissions = 0o600
				}
				if current == bad {
					if wrongOwner {
						observed.Owner = 1000
					} else {
						observed.Permissions = 0o777
					}
				}
				return observed, nil
			}
			if err := validateSourceHostTrustedPath(path, stat); (err == nil) != (bad == "") {
				t.Fatal("protected host path ancestry mismatch", bad, wrongOwner, err)
			}
		}
	}
}

// Even apparently protected synthetic ownership cannot authorize a relative
// path, a symlink, its parent alias or a directory supplied as a marker file.
func TestSourceHostTrustedPathRejectsAliasesAndWrongKinds(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "host.json")
	if err := os.WriteFile(path, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.json")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	stat := func(path string) (os.FileInfo, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		return sourceHostProbeFileInfo{FileInfo: info, Owner: 0, Permissions: 0o755}, nil
	}
	for _, unsafe := range []string{"", "relative.json", "/", root, alias, root + "/../" + filepath.Base(root) + "/host.json", path + "\n"} {
		if err := validateSourceHostTrustedPath(unsafe, stat); err == nil {
			t.Fatal("unsafe host path accepted", unsafe)
		}
	}
}
