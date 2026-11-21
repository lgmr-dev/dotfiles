package targets

func SetFault(f func(point string) error) { fault = f }

func Siblings(d Desired) (staged, aside string) {
	d.Key = Key(d.Path, d.Slot)
	return siblings(d)
}
