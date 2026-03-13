package server

import (
	"testing"

	"encoding/json"

	"github.com/go-playground/assert/v2"
)

func TestJsonCodec(t *testing.T) {
	type Test struct {
		A Id  `json:"a,omitempty"`
		B *Id `json:"b,omitempty"`
	}

	test1 := &Test{}
	test1.A = NewId()
	b_ := NewId()
	test1.B = &b_

	test1Json, err := json.Marshal(test1)
	assert.Equal(t, err, nil)

	test2 := &Test{}
	err = json.Unmarshal(test1Json, test2)
	assert.Equal(t, err, nil)

	assert.Equal(t, test1.A, test2.A)
	assert.Equal(t, test1.B, test2.B)

	test3 := &Test{}
	test3.A = NewId()

	test3Json, err := json.Marshal(test3)
	assert.Equal(t, err, nil)

	test4 := &Test{}
	err = json.Unmarshal(test3Json, test4)
	assert.Equal(t, err, nil)

	assert.Equal(t, test3.A, test4.A)
	assert.Equal(t, test3.B, nil)
	assert.Equal(t, test3.B, test4.B)
}
