//go:build linux

package privateheapprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func directoryOwnedBySelf(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func rootPeer(conn *net.UnixConn) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var credential *unix.Ucred
	var inner error
	if raw.Control(func(fd uintptr) { credential, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }) != nil {
		return false
	}
	return inner == nil && credential != nil && credential.Uid == 0
}

func processCPU() (int64, int64, error) {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0, 0, err
	}
	return usage.Utime.Sec*1e6 + usage.Utime.Usec, usage.Stime.Sec*1e6 + usage.Stime.Usec, nil
}

func currentIdentity(host, block string) (Identity, error) {
	if os.Geteuid() != 0 {
		return Identity{}, errors.New("root_only")
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil || len(stat) > 4096 {
		return Identity{}, errors.New("process_stat_unavailable")
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return Identity{}, errors.New("process_stat_invalid")
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	if len(fields) < 20 {
		return Identity{}, errors.New("process_stat_invalid")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return Identity{}, errors.New("process_start_invalid")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(strings.TrimSpace(string(boot))) != 36 {
		return Identity{}, errors.New("boot_identity_unavailable")
	}
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return Identity{}, errors.New("build_identity_unavailable")
	}
	var revision string
	var modified bool
	modifiedKnown := false
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modifiedKnown = setting.Value == "true" || setting.Value == "false"
			modified = setting.Value == "true"
		}
	}
	if len(revision) != 40 || !modifiedKnown {
		return Identity{}, errors.New("source_identity_unavailable")
	}
	if _, err := hex.DecodeString(revision); err != nil {
		return Identity{}, errors.New("source_identity_invalid")
	}
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		return Identity{}, err
	}
	defer executable.Close()
	info, err := executable.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 256<<20 {
		return Identity{}, errors.New("executable_bound")
	}
	hash := sha256.New()
	if n, err := io.Copy(hash, io.LimitReader(executable, (256<<20)+1)); err != nil || n != info.Size() {
		return Identity{}, errors.New("executable_read_unavailable")
	}
	return Identity{PID: os.Getpid(), StartTicks: start, BootID: strings.TrimSpace(string(boot)), Revision: revision, Modified: modified, ExecutableSHA256: hex.EncodeToString(hash.Sum(nil)), Host: host, Block: block}, nil
}
