package connect

import (
	"encoding/binary"
	mathrand "math/rand/v2"
)

// The generator one connection's writer fills its speed probes from. A probe
// only has to be incompressible and its content is never compared, so nothing
// ties it to the process generator. math/rand.Read builds a probe seven bytes
// at a time under one mutex for the whole process, and a fleet is tested
// together: every provider connection of a platform process that restarts
// gets its speed tests, by default two of 512 probes, about a minute after it
// reconnects. A ChaCha8 for each writer fills a probe in one pass and shares
// no state with another writer.
type speedTestFiller = mathrand.ChaCha8

// Seeded from the process generator of math/rand/v2, whose Uint64 takes no
// lock.
func newSpeedTestFiller() *speedTestFiller {
	var seed [32]byte
	for i := 0; i < len(seed); i += 8 {
		binary.LittleEndian.PutUint64(seed[i:], mathrand.Uint64())
	}
	return mathrand.NewChaCha8(seed)
}
