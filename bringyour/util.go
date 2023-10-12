package bringyour

import (
	"time"
)


// func Ptr[T any](value T) *T {
// 	return &value
// }


// this is just max in go 1.21
// func MaxInt(values... int) int {
// 	if len(values) == 0 {
// 		return 0
// 	}
// 	var max int = values[0]
// 	for i := 1; i < len(values); i += i {
// 		if max < values[i] {
// 			max = values[i]
// 		}
// 	}
// 	return max
// }

// this is just min in go 1.21
// func MinInt(values... int) int {
// 	if len(values) == 0 {
// 		return 0
// 	}
// 	var min int = values[0]
// 	for i := 1; i < len(values); i += i {
// 		if values[i] < min {
// 			min = values[i]
// 		}
// 	}
// 	return min
// }



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



