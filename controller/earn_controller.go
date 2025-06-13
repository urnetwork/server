package controller

import (
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type DateKey = int

func DatetimeStringToDateKey(datetime string) (DateKey, error) {
	t, err := time.Parse("yyyy-mm-dd", datetime)
	if err != nil {
		return 0, err
	}

	// days since the epoch
	return (t.UnixMilli() * time.Millisecond) / (24 * time.Hour)
}

func DateKeyToDatetimeString(dateKey DateKey) string {
	// FIXME
}

type GetEarnDatetimeArgs struct {
	DateKey DateKey
}

type GetEarnDatetimeResult struct {
	Earnings map[string]float64 `json:"earnings"`
}

func GetEarnDatetime(
	args *GetEarnDatetime,
	session *session.ClientSession,
) (*GetEarnDatetimeResult, error) {

	// FIXME return random values

}

type GetEarnDatetimePayoutArgs struct {
	DateKey DateKey
}

type GetEarnDatetimePayoutResult struct {
	TotalPayment float64 `json:"total_payment"`
}

func GetEarnDatetimePayment(
	args *GetEarnDatetimePayment,
	session *session.ClientSession,
) (*GetEarnDatetimePaymentResult, error) {

	// FIXME return random values

}
