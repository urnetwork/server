package router

import (
	// "context"
	"fmt"
	"io"
	"net/http"
)

// implements privacy.txt and other txt resources
func Txt(w http.ResponseWriter, r *http.Request) {
	txtUrl := fmt.Sprintf("https://bringyour.com%s", r.URL.Path)
	http.Redirect(w, r, txtUrl, http.StatusSeeOther)
}

// The api's robots.txt. It keeps crawlers out of /lag/: a crawler that
// renders lag.dev runs its script, and each render would otherwise create a
// lag.dev guest network and open measurement sessions (lag.dev/PLAN.md
// amendment 11). Google's renderer fetches no resource robots.txt disallows.
// Everything else stays as open as it was when this file was a 404.
const RobotsTxtBody = "User-agent: *\nDisallow: /lag/\n"

func RobotsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	io.WriteString(w, RobotsTxtBody)
}
