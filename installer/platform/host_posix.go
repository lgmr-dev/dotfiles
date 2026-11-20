//go:build !windows

package platform

func systemCommandDirectories() ([]string, error) {
	return []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}, nil
}
