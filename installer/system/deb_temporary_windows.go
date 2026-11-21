package system

import "errors"

func debTemporaryIdentity(string) (string, error) {
	return "", errors.New("Linux Code temporary inputs require a POSIX host")
}

func debReadDatabase(string, int) ([]byte, error) {
	return nil, errors.New("Linux Code temporary inputs require a POSIX host")
}
