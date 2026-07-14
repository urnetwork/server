package server

import (
	"context"
	mathrand "math/rand"
	"sync/atomic"
	"time"

	"testing"

	"github.com/urnetwork/connect"
)

func TestPorts(t *testing.T) {

	connect.AssertEqual(t, CollapsePorts([]int{}), "")
	connect.AssertEqual(t, RequireExpandPorts(""), []int{})

	connect.AssertEqual(t, CollapsePorts([]int{1, 2, 3, 5, 7, 8, 10}), "1-3,5,7-8,10")
	connect.AssertEqual(t, RequireExpandPorts("1-3,5,7-8,10"), []int{1, 2, 3, 5, 7, 8, 10})

	connect.AssertEqual(t, CollapsePorts([]int{1, 1, 1, 2}), "1-2")

	connect.AssertEqual(t, RequireExpandPorts("1+2,5,7+1,10"), []int{1, 2, 3, 5, 7, 8, 10})

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
		connect.AssertEqual(t, uniquePorts, uniquePorts2)
	}
}

func TestParseDurationExtended(t *testing.T) {
	valid := []struct {
		in   string
		want time.Duration
	}{
		// day unit
		{"1d", 24 * time.Hour},
		{"2d", 48 * time.Hour},
		{"14d", 14 * 24 * time.Hour},
		// fractional days
		{"1.5d", 36 * time.Hour},
		{".5d", 12 * time.Hour},
		{"0.25d", 6 * time.Hour},
		// days combined with standard units, either order
		{"1d12h", 36 * time.Hour},
		{"12h1d", 36 * time.Hour},
		{"1d1h1m", 24*time.Hour + time.Hour + time.Minute},
		// signs and surrounding whitespace
		{"-1.5d", -36 * time.Hour},
		{"+2d", 48 * time.Hour},
		{" 3d ", 3 * 24 * time.Hour},
		// no day unit: identical to time.ParseDuration
		{"336h", 336 * time.Hour},
		{"90m", 90 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"1.5h", 90 * time.Minute},
		{"500ms", 500 * time.Millisecond},
		{"0", 0},
	}
	for _, tc := range valid {
		got, err := ParseDurationExtended(tc.in)
		if err != nil {
			t.Errorf("ParseDurationExtended(%q) unexpected error: %s", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseDurationExtended(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"d",     // day unit with no value
		"d1",    // value after the unit
		"1.5",   // no unit at all
		"abc",   // not a duration
		"1x",    // unknown unit
		"1.5dd", // malformed day component
	}
	for _, in := range invalid {
		if _, err := ParseDurationExtended(in); err == nil {
			t.Errorf("ParseDurationExtended(%q) expected an error, got nil", in)
		}
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
	connect.AssertEqual(t, int(count.Load()), 16)
}
