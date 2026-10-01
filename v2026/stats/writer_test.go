package stats

import "testing"

func TestStreamWriterQueueByteReservation(t *testing.T) {
	writer := &streamWriter{
		settings: &streamWriterSettings{maxQueueBytes: 5},
	}
	if !writer.reserveQueueBytes(4) {
		t.Fatal("initial reservation failed")
	}
	if writer.reserveQueueBytes(2) {
		t.Fatal("reservation exceeding byte cap succeeded")
	}
	writer.queuedBytes.Add(-4)
	if !writer.reserveQueueBytes(5) {
		t.Fatal("reservation at byte cap failed")
	}
	if writer.reserveQueueBytes(1) {
		t.Fatal("reservation beyond a full byte cap succeeded")
	}
}
