package platform

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// CreateDirectoryLink makes link point at the folder target: a symbolic link
// on POSIX and a junction on Windows. A junction needs no Developer Mode and
// no symbolic link privilege.
// https://learn.microsoft.com/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_reparse_data_buffer
func CreateDirectoryLink(target, link string) (result error) {
	if !filepath.IsAbs(target) {
		return errors.New("junction target must be absolute")
	}
	substitute := `\??\` + target
	if strings.HasPrefix(target, `\\`) {
		substitute = `\??\UNC\` + strings.TrimPrefix(target, `\\`)
	}
	sub, err := windows.UTF16FromString(substitute)
	if err != nil {
		return err
	}
	print, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	size := 16 + 2*(len(sub)+len(print))
	if size > 16384 {
		return errors.New("junction target exceeds the native reparse buffer limit")
	}
	buffer := make([]byte, size)
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(size-8))
	binary.LittleEndian.PutUint16(buffer[10:12], uint16(2*(len(sub)-1)))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(2*len(sub)))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16(2*(len(print)-1)))
	for i, value := range append(sub, print...) {
		binary.LittleEndian.PutUint16(buffer[16+2*i:], value)
	}
	name, err := windows.UTF16PtrFromString(WindowsExtendedPath(link))
	if err != nil {
		return err
	}
	if err := os.Mkdir(link, 0700); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, os.Remove(link))
		}
	}()
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, windows.CloseHandle(handle)) }()
	var returned uint32
	return windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil)
}

// ResolvePath returns the physical path of an existing path. On Windows it
// also follows junctions, which filepath.EvalSymlinks does not: it asks the
// open handle for its final DOS path, UNC paths included.
func ResolvePath(path string) (result string, resultErr error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("native path resolution requires an absolute path")
	}
	name, err := windows.UTF16PtrFromString(WindowsExtendedPath(path))
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	buffer := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return "", err
	}
	if n == 0 || n >= uint32(len(buffer)) {
		return "", errors.New("resolved Windows path exceeds native path limit")
	}
	result = windows.UTF16ToString(buffer[:n])
	if strings.HasPrefix(result, `\\?\UNC\`) {
		result = `\\` + strings.TrimPrefix(result, `\\?\UNC\`)
	} else {
		result = strings.TrimPrefix(result, `\\?\`)
	}
	if !filepath.IsAbs(result) {
		return "", errors.New("native path resolution returned a non-absolute path")
	}
	return filepath.Clean(result), nil
}
