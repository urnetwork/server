package handlers

import (
    "net/http"

    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/router"
)


func PreferencesSet(w http.ResponseWriter, r *http.Request) {
    router.WrapWithInputRequireAuth(model.PreferencesSet, w, r)	
}


func FeedbackSend(w http.ResponseWriter, r *http.Request) {
    router.WrapWithInputRequireAuth(model.FeedbackSend, w, r)   
}

