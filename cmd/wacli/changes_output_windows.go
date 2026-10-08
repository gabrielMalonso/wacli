package main

import "os"

func changesWatchOutputFile(stdout *os.File) (*os.File, func(), error) {
	return stdout, func() {}, nil
}
