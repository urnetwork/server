package handlers

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// SnTop backs `GET /sn/top` (LEADERTOP.md §7): the SN Top board's top 400,
// keyset paged within one snapshot. Public; a signed-in caller also gets its
// own row.
func SnTop(w http.ResponseWriter, r *http.Request) {
	args, err := snTopArgsFromQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	impl := func(clientSession *session.ClientSession) (*controller.SnTopResult, error) {
		return controller.GetSnTop(args, clientSession)
	}
	router.WrapOptionalAuth(impl, w, r)
}

// SnTopMe backs `GET /sn/top/me`: the caller network's detail and history.
func SnTopMe(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.GetSnTopMe, w, r)
}

func snTopArgsFromQuery(query url.Values) (*controller.SnTopArgs, error) {
	args := &controller.SnTopArgs{
		Cursor:   query.Get("cursor"),
		Filter:   query.Get("filter"),
		Snapshot: query.Get("snapshot"),
	}
	if limit := query.Get("limit"); limit != "" {
		value, err := strconv.Atoi(limit)
		if err != nil || value < 0 {
			return nil, errors.New("Bad limit.")
		}
		args.Limit = value
	}
	return args, nil
}
