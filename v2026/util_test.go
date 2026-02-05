package server

import (
	mathrand "math/rand"

	"testing"

	"github.com/go-playground/assert/v2"
)

func TestPorts(t *testing.T) {

	assert.Equal(t, CollapsePorts([]int{}), "")
	assert.Equal(t, RequireExpandPorts(""), []int{})

	assert.Equal(t, CollapsePorts([]int{1, 2, 3, 5, 7, 8, 10}), "1-3,5,7-8,10")
	assert.Equal(t, RequireExpandPorts("1-3,5,7-8,10"), []int{1, 2, 3, 5, 7, 8, 10})

	assert.Equal(t, CollapsePorts([]int{1, 1, 1, 2}), "1-2")

	assert.Equal(t, RequireExpandPorts("1+2,5,7+1,10"), []int{1, 2, 3, 5, 7, 8, 10})

	for range 1024 {
		n := mathrand.Intn(128)

		ports := []int{}
		uniquePorts := map[int]bool{}
		for range n {
			p := mathrand.Intn(32)
			ports = append(ports, p)
			uniquePorts[p] = true
		}

		ports2 := RequireExpandPorts(CollapsePorts(ports))
		uniquePorts2 := map[int]bool{}
		for _, port := range ports2 {
			uniquePorts2[port] = true
		}
		assert.Equal(t, uniquePorts, uniquePorts2)
	}
}
