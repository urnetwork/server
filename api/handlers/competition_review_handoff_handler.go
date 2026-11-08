package handlers

import (
	"github.com/urnetwork/server/controller"
	"net/http"
)

func CompetitionV2ReviewHandoffImport(w http.ResponseWriter, r *http.Request) {
	controller.CompetitionV2ReviewHandoffImportHandler(w, r)
}
