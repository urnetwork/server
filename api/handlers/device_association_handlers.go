package handlers

import (
	"net/http"

	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
)


func DeviceAdd(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.DeviceAdd, w, r)
}


func DeviceCreateShareCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.DeviceCreateShareCode, w, r)
}


func DeviceShareCodeQR(w http.ResponseWriter, r *http.Request) {
	pathValues := router.GetPathValues(r)

	shareCodeQR := &model.DeviceShareCodeQRArgs{
		ShareCode: pathValues[0],
	}
	impl := func(clientSession *session.ClientSession)(*model.DeviceShareCodeQRResult, error) {
		return model.DeviceShareCodeQR(shareCodeQR, clientSession)
	}
	router.WrapRequireAuth(
		impl,
		w,
		r,
		func(result *model.DeviceShareCodeQRResult)(bool) {
			w.Header().Set("Content-Type", "image/png")
		    w.Write(result.PngBytes)
		    return true
		},
	)
}


func DeviceShareStatus(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.DeviceShareStatus, w, r)
}


func DeviceConfirmShare(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.DeviceConfirmShare, w, r)
}


func DeviceCreateAdoptCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.DeviceCreateAdoptCode, w, r)
}


func DeviceAdoptCodeQR(w http.ResponseWriter, r *http.Request) {
	pathValues := router.GetPathValues(r)

	adoptCodeQR := &model.DeviceAdoptCodeQRArgs{
		AdoptCode: pathValues[0],
	}
	impl := func(clientSession *session.ClientSession)(*model.DeviceAdoptCodeQRResult, error) {
		return model.DeviceAdoptCodeQR(adoptCodeQR, clientSession)
	}
	router.WrapNoAuth(
		impl,
		w,
		r,
		func(result *model.DeviceAdoptCodeQRResult)(bool) {
			w.Header().Set("Content-Type", "image/png")
		    w.Write(result.PngBytes)
		    return true
		},
	)
}


func DeviceAdoptStatus(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.DeviceAdoptStatus, w, r)
}


func DeviceConfirmAdopt(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.DeviceConfirmAdopt, w, r)
}


func DeviceRemoveAdoptCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.DeviceRemoveAdoptCode, w, r)
}


func DeviceAssociations(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.DeviceAssociations, w, r)
}


func DeviceRemoveAssociation(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.DeviceRemoveAssociation, w, r)
}


func DeviceSetAssociationName(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.DeviceSetAssociationName, w, r)
}


