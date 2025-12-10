package server

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
	// "runtime/debug"
	"bytes"
	"encoding/json"
	"strings"
	// "reflect"
	// "runtime"
	"slices"
	"sync"
)

// func Ptr[T any](value T) *T {
//  return &value
// }

// this is just max in go 1.21
// func MaxInt(values... int) int {
//  if len(values) == 0 {
//      return 0
//  }
//  var max int = values[0]
//  for i := 1; i < len(values); i += i {
//      if max < values[i] {
//          max = values[i]
//      }
//  }
//  return max
// }

// this is just min in go 1.21
// func MinInt(values... int) int {
//  if len(values) == 0 {
//      return 0
//  }
//  var min int = values[0]
//  for i := 1; i < len(values); i += i {
//      if values[i] < min {
//          min = values[i]
//      }
//  }
//  return min
// }

func NowUtc() time.Time {
	// data stores use utc time without time zone
	// use the same time format locally to keep the local time in sync with the data store time
	return time.Now().UTC()
}

func CodecTime(t time.Time) time.Time {
	// nanosecond resolution can be serialized and unserialized in most codecs:
	// - json
	// - postgres
	return t.Round(time.Nanosecond)
}

func MinTime(a time.Time, b time.Time) time.Time {
	if a.Before(b) {
		return a
	} else {
		return b
	}
}

func MaxTime(a time.Time, b time.Time) time.Time {
	if a.Before(b) {
		return b
	} else {
		return a
	}
}

func Raise(err error) {
	if err != nil {
		panic(err)
	}
}

// returns source if cannot compact
func AttemptCompactJson(jsonBytes []byte) []byte {
	b := &bytes.Buffer{}
	if err := json.Compact(b, jsonBytes); err == nil {
		return b.Bytes()
	} else {
		// there was an error compacting the json
		// return the original
		return jsonBytes
	}
}

func MaskValue(v string) string {
	if len(v) < 6 {
		return "***"
	} else {
		return fmt.Sprintf("%s***%s", v[:2], v[len(v)-2:])
	}
}

var portRangeRegex = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile("^\\s*(\\d+)\\s*-\\s*(\\d+)\\s*$")
})
var portRange2Regex = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile("^\\s*(\\d+)\\s*\\+\\s*(\\d+)\\s*$")
})
var portRegex = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile("^\\s*(\\d+)\\s*$")
})

func ExpandPorts(portsListStr string) ([]int, error) {
	if portsListStr == "" {
		return []int{}, nil
	}

	ports := []int{}
	for _, portsStr := range strings.Split(portsListStr, ",") {
		if portStrs := portRangeRegex().FindStringSubmatch(portsStr); portStrs != nil {
			minPort, err := strconv.Atoi(portStrs[1])
			if err != nil {
				panic(err)
			}
			maxPort, err := strconv.Atoi(portStrs[2])
			if err != nil {
				panic(err)
			}
			for port := minPort; port <= maxPort; port += 1 {
				ports = append(ports, port)
			}
		} else if portStrs := portRange2Regex().FindStringSubmatch(portsStr); portStrs != nil {
			minPort, err := strconv.Atoi(portStrs[1])
			if err != nil {
				panic(err)
			}
			n, err := strconv.Atoi(portStrs[2])
			if err != nil {
				panic(err)
			}
			maxPort := minPort + n
			for port := minPort; port <= maxPort; port += 1 {
				ports = append(ports, port)
			}
		} else if portStrs := portRegex().FindStringSubmatch(portsStr); portStrs != nil {
			port, err := strconv.Atoi(portStrs[1])
			if err != nil {
				panic(err)
			}
			ports = append(ports, port)
		} else {
			return nil, fmt.Errorf("Port must be either int min-max or int port (%s)", portsStr)
		}
	}
	return ports, nil
}

func RequireExpandPorts(portsListStr string) []int {
	ports, err := ExpandPorts(portsListStr)
	if err != nil {
		panic(err)
	}
	return ports
}

func CollapsePorts(ports []int) string {
	parts := []string{}

	slices.Sort(ports)
	for i := 0; i < len(ports); {
		j := i + 1
		for j < len(ports) && (ports[j] == ports[j-1]+1 || ports[j] == ports[j-1]) {
			j += 1
		}
		if ports[i] == ports[j-1] {
			parts = append(parts, fmt.Sprintf("%d", ports[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ports[i], ports[j-1]))
		}
		i = j
	}

	return strings.Join(parts, ",")
}
