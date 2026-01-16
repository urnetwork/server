package router

import (
	// "context"
	"fmt"
	"net/http"
)

// implements privacy.txt and other txt resources
func Txt(w http.ResponseWriter, r *http.Request) {
	txtUrl := fmt.Sprintf("https://bringyour.com%s", r.URL.Path)
	http.Redirect(w, r, txtUrl, http.StatusSeeOther)
}
