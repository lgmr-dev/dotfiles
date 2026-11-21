package platform

import "os"

// SyncDirectory flushes a folder to disk. It does nothing on Windows, where a
// directory handle cannot be flushed.
func SyncDirectory(string) error { return nil }

func syncHandle(*os.File) error { return nil }
