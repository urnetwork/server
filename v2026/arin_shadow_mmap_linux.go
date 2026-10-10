package server

import (
	"os"
	"strconv"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func openMappedArinShadowFile(file *os.File) (*mmdb.Reader, error) {
	return mmdb.Open("/proc/self/fd/" + strconv.FormatUint(uint64(file.Fd()), 10))
}
