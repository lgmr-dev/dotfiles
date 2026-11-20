package v2import

func SetFault(f func(point string) error) { fault = f }
