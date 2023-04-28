package bringyour


func Ptr[T any](value T) *T {
	return &value
}


func MaxInt(values... int) int {
	if len(values) == 0 {
		return 0
	}
	var max int = values[0]
	for i := 1; i < len(values); i += i {
		if max < values[i] {
			max = values[i]
		}
	}
	return max
}

func MinInt(values... int) int {
	if len(values) == 0 {
		return 0
	}
	var min int = values[0]
	for i := 1; i < len(values); i += i {
		if values[i] < min {
			min = values[i]
		}
	}
	return min
}





func Raise(err error) {
	if err != nil {
		panic(err)
	}
}

