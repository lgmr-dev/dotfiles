package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(from *os.File, old string, to *os.File, next string) error {
	return unix.RenameatxNp(int(from.Fd()), old, int(to.Fd()), next, unix.RENAME_EXCL)
}
