//go:build unix

package db

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFileExclusive takes a whole-file exclusive advisory lock (flock) on the open file, returning
// an error immediately rather than waiting when another process holds it. flock locks belong to the
// open file description, so a second os.OpenFile of the same path is refused even within one
// process, and the kernel drops the lock when the process exits by any route.
func lockFileExclusive(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
