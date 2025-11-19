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

	impl := func(sess *session.ClientSession) (*controller.UploadLogFileResult, error) {
		// r.Body is a streaming reader; DO NOT read it fully here.
		// Ensure we respect cancellation.
		ctx := r.Context()
		return controller.UploadLogFile(ctx, r.Body, controller.UploadLogFileOptions{
			// OriginalFilename: originalName,
			FeedbackId:  feedbackId,
			ContentType: contentType,
			NetworkId:   sess.ByJwt.NetworkId,
			UserId:      sess.ByJwt.UserId,
			ClientId:    sess.ByJwt.ClientId,
			Now:         time.Now(),
		})
	}

	router.WrapRequireAuth(impl, w, r)
}
