//go:build !linux

package server

import (
	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"os"
)

// Main's capture runtime is Linux. Refuse a mapping authority which cannot
// reopen the exact validated descriptor; the existing copied reader is portable.
func openMappedArinShadowFile(*os.File) (*mmdb.Reader, error) { return nil, ErrArinShadowInput }
