package platform

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// DirectoryIdentity returns the volume and file identity of an open folder.
func DirectoryIdentity(file *os.File) (string, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

// FileOwner returns the user, the group and the mode bits of path without
// following a link. It fails on Windows, which has no POSIX ownership.
func FileOwner(path string) (uint32, uint32, uint32, error) {
	return 0, 0, 0, fmt.Errorf("POSIX ownership is unavailable for %s on Windows", path)
}
