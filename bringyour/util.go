package bringyour


func Ptr[T any](value T) *T {
	return &value
}

