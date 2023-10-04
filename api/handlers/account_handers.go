package handlers

import (
    "context"
    "net/http"

    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/router"
)


func PreferencesSet(w http.ResponseWriter, r *http.Request) {
    router.WrapWithJson(ctx, model.PreferencesSet, w, r)	
}


func FeedbackSend(w http.ResponseWriter, r *http.Request) {
    router.WrapWithJson(ctx, model.FeedbackSend, w, r)   
}

