package bringyour

import (
    "time"
    "regexp"
    "fmt"
    "strconv"
    "runtime/debug"
    "strings"
    "encoding/json"
    "bytes"
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






func Raise(err error) {
    if err != nil {
        panic(err)
    }
}


// this is meant to handle unexpected errors and do some cleanup
func HandleError(do func(), handlers ...func()) {
    defer func() {
        if err := recover(); err != nil {
            Logger().Printf("Unexpected error: %s\n", ErrorJson(err, debug.Stack()))
            for _, handler := range handlers {
                handler()
            }
        }
    }()
    do()
}



func ParseClientAddress(clientAddress string) (ip string, port int, err error) {
    // ipv4:port
    // [ipv6]:port
    // ipv6:port

    ipv4 := regexp.MustCompile("^([0-9\\.]+):(\\d+)$")
    ipv6 := regexp.MustCompile("^\\[([0-9a-f:]+)\\]:(\\d+)$")
    // ip not properly escaped with [...]
    badIpv6 := regexp.MustCompile("^([0-9a-f:]+):(\\d+)$")
    
    groups := ipv4.FindStringSubmatch(clientAddress)
    if groups != nil {
        ip = groups[1]
        port, _ = strconv.Atoi(groups[2])
        return
    }

    groups = ipv6.FindStringSubmatch(clientAddress)
    if groups != nil {
        ip = groups[1]
        port, _ = strconv.Atoi(groups[2])
        return
    }

    groups = badIpv6.FindStringSubmatch(clientAddress)
    if groups != nil {
        ip = groups[1]
        port, _ = strconv.Atoi(groups[2])
        return
    }

    err = fmt.Errorf("Client address does not match ipv4 or ipv6 spec: %s", clientAddress)
    return
}


func ErrorJson(err any, stack []byte) string {
    stackLines := []string{}
    for _, line := range strings.Split(string(stack), "\n") {
        stackLines = append(stackLines, strings.TrimSpace(line))
    }
    errorJson, _ := json.Marshal(map[string]any{
        "error": fmt.Sprintf("%s", err),
        "stack": stackLines,
    })
    return string(errorJson)
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

