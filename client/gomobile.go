package client

import (
	"encoding/json"
	"reflect"
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
	gmLog("GOMOBILE UNMARSHAL: %s", string(b))
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


type PathList struct {
	exportedList[*Path]
}

func NewPathList() *PathList {
	return &PathList{
		exportedList: *newExportedList[*Path](),
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


type LocationResultList struct {
	exportedList[*LocationResult]
}

func NewLocationResultList() *LocationResultList {
	return &LocationResultList{
		exportedList: *newExportedList[*LocationResult](),
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
