package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func LogUpload(w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}

	var feedbackId *server.Id
	if fid := r.Header.Get("X-UR-Feedback-Id"); fid != "" {
		if id, err := server.ParseId(fid); err == nil {
			feedbackId = &id
		} else {
			http.Error(w, "invalid X-UR-Feedback-Id", http.StatusBadRequest)
			return
		}
	}

	impl := func(session *session.ClientSession) (*controller.UploadLogFileResult, error) {
		// r.Body is a streaming reader; DO NOT read it fully here.
		// Ensure we respect cancellation.
		// ctx := r.Context()
		return controller.UploadLogFile(session, r.Body, controller.UploadLogFileArgs{
			// OriginalFilename: originalName,
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
