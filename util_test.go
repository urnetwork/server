package server

import (
	"context"
	mathrand "math/rand"
	"sync/atomic"

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

func TestRunPosts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var count atomic.Uint64
	var f func(n int) PostFunction
	f = func(n int) PostFunction {
		return func() any {
			var posts []PostFunction
			if n <= 1 {
				count.Add(1)
				return nil
			} else {
				for i := range n {
					posts = append(posts, f(i))
				}
				if len(posts) == 1 {
					return posts[0]
				} else {
					return posts
				}
			}
		}
	}

	// f(0) -> 1
	// f(1) -> f(0)
	// f(2) -> f(1) + f(0) = 2 * f(1) = 2
	// f(3) -> f(2) + f(1) + f(0) = 2 * f(2) = 2^2
	// f(4) -> f(3) + f(2) + f(1) + f(0) = 2 * f(3) = 2^3

	RunPosts(ctx, f(5))
	assert.Equal(t, int(count.Load()), 16)
}
