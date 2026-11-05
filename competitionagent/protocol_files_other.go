//go:build !linux

// Portable callers retain signed envelopes without receiving an implicitly
// weaker file backend. Restricted subevaluation execution is Linux-only.
package competitionagent

import (
	"errors"
	"io"
)

var errFileTransportUnsupported = errors.New("restricted subevaluation file transport requires Linux")

// Refusal precedes peer writes, source opens and any filesystem mutation.
func WriteFiles(writer io.Writer, root string, files []File, maximum int64, limits Limits) error {
	return errFileTransportUnsupported
}

// Non-Linux builds never replace raw descriptor ownership with relative-link
// following or claim that compiling a controller qualifies another executor.
func ReadFiles(reader io.Reader, root string, files []File, maximum int64, limits Limits) error {
	return errFileTransportUnsupported
}
