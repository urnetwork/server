package client

import (
	"encoding/json"
)


// use a exported lists since arrays of structs are not exportable
// (see notes in `client.go`)



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
