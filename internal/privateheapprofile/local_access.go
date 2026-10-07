package privateheapprofile

import (
	"net"
	"os"
)

// LocalIdentity shares the existing native process attestation with other
// private diagnostic owners. It reads only the current process, never a peer.
func LocalIdentity(host, block string) (Identity, error) { return currentIdentity(host, block) }

// RootPeer verifies the kernel credential on an accepted Unix connection.
func RootPeer(conn *net.UnixConn) bool { return rootPeer(conn) }

// OwnsPrivateDirectory checks ownership in addition to the caller's mode and
// non-symlink checks. Unsupported platforms refuse private diagnostics.
func OwnsPrivateDirectory(info os.FileInfo) bool { return directoryOwnedBySelf(info) }
