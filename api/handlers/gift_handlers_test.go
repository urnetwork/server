package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/controller"
)

// POST /pay/gift/cards.pdf answers the cards as a download that nothing may
// keep a copy of: they carry the codes.
func TestGiftCardsPdfIsADownload(t *testing.T) {
	pdf := []byte("%PDF-1.7 synthetic gift cards")
	w := httptest.NewRecorder()
	writeGiftCardsPdf(w, &controller.GiftCardsPdfResult{Pdf: pdf})
	for header, want := range map[string]string{
		"Content-Type":           "application/pdf",
		"Content-Disposition":    `attachment; filename="URnetwork-gift-cards.pdf"`,
		"Content-Length":         "29",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if w.Body.String() != string(pdf) {
		t.Errorf("body = %q", w.Body.String())
	}
}
