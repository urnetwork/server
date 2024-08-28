package client

import (
	"encoding/json"
	"reflect"
	"time"
)

// use a exported lists since arrays of structs are not exportable
// (see notes in `client.go`)

var gmLog = logFn("gomobile")

// conforms to `json.Marshaler` and `json.Unmarshaler`
type exportedList[T any] struct {
	values []T
}

func newExportedList[T any]() *exportedList[T] {
	return &exportedList[T]{}
}

func (self *exportedList[T]) Len() int {
	return len(self.values)
}

func (self *exportedList[T]) Get(i int) T {
	return self.values[i]
}

func (self *exportedList[T]) Add(value T) {
	self.values = append(self.values, value)
}

func (self *exportedList[T]) addAll(values ...T) {
	self.values = append(self.values, values...)
}

func (self *exportedList[T]) Contains(v T) bool {
	for _, value := range self.values {
		if reflect.DeepEqual(value, v) {
			return true
		}
	}
	return false
}

func (self *exportedList[T]) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, &self.values)
}

func (self *exportedList[T]) MarshalJSON() ([]byte, error) {
	return json.Marshal(self.values)
}

type StringList struct {
	exportedList[string]
}

func NewStringList() *StringList {
	return &StringList{
		exportedList: *newExportedList[string](),
	}
}

type IdList struct {
	exportedList[*Id]
}

func NewIdList() *IdList {
	return &IdList{
		exportedList: *newExportedList[*Id](),
	}
}

type IntList struct {
	exportedList[int]
}

func NewIntList() *IntList {
	return &IntList{
		exportedList: *newExportedList[int](),
	}
}

type TransferPathList struct {
	exportedList[*TransferPath]
}

func NewTransferPathList() *TransferPathList {
	return &TransferPathList{
		exportedList: *newExportedList[*TransferPath](),
	}
}

type ProviderSpecList struct {
	exportedList[*ProviderSpec]
}

func NewProviderSpecList() *ProviderSpecList {
	return &ProviderSpecList{
		exportedList: *newExportedList[*ProviderSpec](),
	}
}

type FindProvidersProviderList struct {
	exportedList[*FindProvidersProvider]
}

func NewFindProvidersProviderList() *FindProvidersProviderList {
	return &FindProvidersProviderList{
		exportedList: *newExportedList[*FindProvidersProvider](),
	}
}

type GridPointList struct {
	exportedList[*GridPoint]
}

func NewGridPointList() *GridPointList {
	return &GridPointList{
		exportedList: *newExportedList[*GridPoint](),
	}
}

type LocationResultList struct {
	exportedList[*LocationResult]
}

func NewLocationResultList() *LocationResultList {
	return &LocationResultList{
		exportedList: *newExportedList[*LocationResult](),
	}
}

type LocationGroupResultList struct {
	exportedList[*LocationGroupResult]
}

func NewLocationGroupResultList() *LocationGroupResultList {
	return &LocationGroupResultList{
		exportedList: *newExportedList[*LocationGroupResult](),
	}
}

type LocationDeviceResultList struct {
	exportedList[*LocationDeviceResult]
}

func NewLocationDeviceResultList() *LocationDeviceResultList {
	return &LocationDeviceResultList{
		exportedList: *newExportedList[*LocationDeviceResult](),
	}
}

type ConnectLocationList struct {
	exportedList[*ConnectLocation]
}

func NewConnectLocationList() *ConnectLocationList {
	return &ConnectLocationList{
		exportedList: *newExportedList[*ConnectLocation](),
	}
}

type NetworkClientInfoList struct {
	exportedList[*NetworkClientInfo]
}

func NewNetworkClientInfoList() *NetworkClientInfoList {
	return &NetworkClientInfoList{
		exportedList: *newExportedList[*NetworkClientInfo](),
	}
}

type NetworkClientConnectionList struct {
	exportedList[*NetworkClientConnection]
}

func NewNetworkClientConnectionList() *NetworkClientConnectionList {
	return &NetworkClientConnectionList{
		exportedList: *newExportedList[*NetworkClientConnection](),
	}
}

type TransferBalanceList struct {
	exportedList[*TransferBalance]
}

func NewTransferBalanceList() *TransferBalanceList {
	return &TransferBalanceList{
		exportedList: *newExportedList[*TransferBalance](),
	}
}

// conforms to `json.Marshaler` and `json.Unmarshaler`
type Time struct {
	impl time.Time
}

func NewTimeUnixMilli(unixMilli int64) *Time {
	return &Time{
		impl: time.UnixMilli(unixMilli),
	}
}

func newTime(impl time.Time) *Time {
	return &Time{
		impl: impl,
	}
}

func (self *Time) UnixMilli() int64 {
	return self.impl.UnixMilli()
}

func (self *Time) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, &self.impl)
}

func (self *Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(self.impl)
}
