package targets

func SetFault(f func(point string) error) { fault = f }
