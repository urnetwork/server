package bringyour


func Ptr[T any](value T) *T {
	return &value
}


type Closeable interface {
	Close()
}

func With[T Closeable](t T, err error, callback func()) {
	Raise(err)
	defer t.Close()
	callback()
}

func Raise(err error) {
	if err != nil {
		panic(err)
	}
}

