package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/router"
	"github.com/urnetwork/server/v2025/session"
)

func LogUpload(w http.ResponseWriter, r *http.Request) {

	contentType := r.Header.Get("Content-Type")

	var feedbackId *server.Id
	pathValues := router.GetPathValues(r)
	if len(pathValues) == 0 {
		http.Error(w, "missing feedback id in path", http.StatusBadRequest)
		return
	}

	if id, err := server.ParseId(pathValues[0]); err == nil {
		feedbackId = &id
	} else {
		http.Error(w, "invalid feedback id passed", http.StatusBadRequest)
		return
	}

	impl := func(session *session.ClientSession) (*controller.UploadLogFileResult, error) {
		// r.Body is a streaming reader; DO NOT read it fully here.
		// Ensure we respect cancellation.
		return controller.UploadLogFile(session, r.Body, controller.UploadLogFileArgs{
			FeedbackId:  feedbackId,
			ContentType: contentType,
			NetworkId:   session.ByJwt.NetworkId,
			UserId:      session.ByJwt.UserId,
			ClientId:    session.ByJwt.ClientId,
			Now:         time.Now(),
		})
	}

	router.WrapRequireAuth(impl, w, r)
}
