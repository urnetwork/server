package handlers

import (
    "net/http"

    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
    "bringyour.com/bringyour/router"
)


func NetworkCheck(w http.ResponseWriter, r *http.Request) {
    router.WrapWithInputNoAuth(model.NetworkCheck, w, r)
}


func NetworkCreate(w http.ResponseWriter, r *http.Request) {
    router.WrapWithInputNoAuth(controller.NetworkCreate, w, r)
}

