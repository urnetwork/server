// Collector-only descriptor hashing owns its read bound and cancellation.
// Existing admission/execution helpers retain their historical behavior.
package competitionagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"syscall"
)

// Reuse the existing concrete native observations without inheriting its
// unbounded legacy executable validator for new collection work.
type nativeQualificationNativeHost struct{ runtimeBoundaryNativeHost }

// This descriptor interface is private and exists for deterministic mutation
// and read-error controls; production always supplies an opened regular file.
type nativeQualificationExecutableFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
}

// A checked pathname and its open descriptor must still identify the same
// physical protected file after the bounded hash; no program is executed here.
func (nativeQualificationNativeHost) Executable(ctx context.Context, pin Pin, maximum int64) (count int64, resultErr error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if maximum <= 0 || maximum == math.MaxInt64 || !digestPattern.MatchString(pin.Sha256) {
		return 0, errors.New("collector executable omitted finite pinned identity")
	}
	if err := protectedRootPath(pin.Path, false, false); err != nil {
		return 0, err
	}
	file, err := openRegularNoFollow(pin.Path)
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	pathInfo := func() (os.FileInfo, error) {
		if err := protectedRootPath(pin.Path, false, false); err != nil {
			return nil, err
		}
		return os.Lstat(pin.Path)
	}
	return hashNativeQualificationExecutable(ctx, file, pathInfo, pin, maximum, true)
}

// contextReader and openRegularNoFollow are existing bounded-I/O utilities.
// Cancellation between reads preserves its original identity via errors.Join;
// a blocked kernel read still requires the outer absolute unit deadline.
func hashNativeQualificationExecutable(ctx context.Context, file nativeQualificationExecutableFile, pathInfo func() (os.FileInfo, error), pin Pin, maximum int64, protected bool) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if maximum <= 0 || maximum == math.MaxInt64 || !digestPattern.MatchString(pin.Sha256) || file == nil || pathInfo == nil {
		return 0, errors.New("collector executable omitted finite descriptor authority")
	}
	before, err := file.Stat()
	if err != nil {
		return 0, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Mode().Perm()&0111 == 0 || before.Mode().Perm()&0022 != 0 || before.Size() <= 0 || before.Size() > maximum || stat.Nlink != 1 || protected && stat.Uid != 0 {
		return 0, errors.New("collector executable descriptor is unsafe or outside bound")
	}
	beforeStat := *stat
	digest := sha256.New()
	count, readErr := io.Copy(digest, &contextReader{ctx: ctx, reader: io.LimitReader(file, maximum+1)})
	if readErr != nil || ctx.Err() != nil {
		return count, errors.Join(readErr, ctx.Err())
	}
	after, statErr := file.Stat()
	current, pathErr := pathInfo()
	if statErr != nil || pathErr != nil {
		return count, errors.Join(statErr, pathErr)
	}
	sameMetadata := func(info os.FileInfo) bool {
		other, ok := info.Sys().(*syscall.Stat_t)
		return ok && other.Dev == beforeStat.Dev && other.Ino == beforeStat.Ino && before.Size() == info.Size() && before.Mode() == info.Mode() && before.ModTime().Equal(info.ModTime()) && beforeStat.Ctim == other.Ctim && beforeStat.Nlink == other.Nlink && beforeStat.Uid == other.Uid && beforeStat.Gid == other.Gid && beforeStat.Mode == other.Mode
	}
	if count != before.Size() || count > maximum || hex.EncodeToString(digest.Sum(nil)) != pin.Sha256 || !sameMetadata(after) || !sameMetadata(current) {
		return count, errors.New("collector executable changed, truncated, or differs from its pin")
	}
	return count, ctx.Err()
}
