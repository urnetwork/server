package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

func GetEarnDatetime(w http.ResponseWriter, r *http.Request) {
	pathValues := router.GetPathValues(r)

	// regex
	re := RequireCompile("[0-9]{4,4}-[0-9]{2,2}-[0-9]{2,2}")
	if !re.MatchString(pathValues[0]) {
		writeError(fmt.Sprintf("Bad datetime: %s. Expect yyyy-mm-dd.", pathValues[0]))
	}

	shareCodeQR := &model.DeviceShareCodeQRArgs{
		ShareCode: pathValues[0],
	}
	impl := func(clientSession *session.ClientSession) (*model.DeviceShareCodeQRResult, error) {
		return model.DeviceShareCodeQR(shareCodeQR, clientSession)
	}

	router.WrapWithInputNoAuth(controller.GetEarnDatetime, w, r)
}

func GetEarnDatetimePayout(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.GetEarnDatetimePayout, w, r)
}
