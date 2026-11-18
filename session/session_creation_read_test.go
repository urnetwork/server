package session

import (
	"testing"
	"time"
)

// S1 W10: session_creation_enabled answered every mint from a fresh read and
// parse of vault auth.yml, and a read that panicked (a file that failed to
// parse) panicked the mint. Reads are now bounded per interval, and a failed
// read keeps the last value.
func TestSessionCreationReadIsBoundedAndSurvivesAFailedRead(t *testing.T) {
	previousRead := readSessionCreationSetting
	previousCache := sessionCreationCache.Swap(nil)
	defer func() {
		readSessionCreationSetting = previousRead
		sessionCreationCache.Store(previousCache)
	}()
	reads := 0
	readSessionCreationSetting = func() bool {
		reads += 1
		return true
	}
	for range 100 {
		if !SessionCreationEnabled() {
			t.Fatal("the setting was not read")
		}
	}
	if reads != 1 {
		t.Fatal("every call read the vault", reads)
	}
	// the interval passes, and the file fails to parse
	sessionCreationCache.Store(&sessionCreationRead{value: true, until: time.Now().Add(-time.Second)})
	readSessionCreationSetting = func() bool { panic("synthetic auth.yml parse failure") }
	if !SessionCreationEnabled() {
		t.Fatal("a failed read lost the last value")
	}
}
