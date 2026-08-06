//go:build windows

package db

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockRegionOffsetHigh puts the locked byte range far past any plausible end of the lock file.
// Windows byte-range locks are mandatory for reads and writes of the locked range, so locking byte
// zero would stop the losing process reading the holder details it is about to report; a byte
// beyond the content excludes just as well and leaves the file legible. Locking past EOF is
// explicitly permitted.
const lockRegionOffsetHigh = 0x4000_0000

// lockFileExclusive takes an exclusive byte-range lock on the open file, returning an error
// immediately rather than waiting when another process holds it. Windows associates the lock with
// the file handle - a second handle is refused even within the locking process, matching flock's
// behaviour on unix - and releases it when the handle is closed, including on process exit.
func lockFileExclusive(file *os.File) error {
	overlapped := &windows.Overlapped{OffsetHigh: lockRegionOffsetHigh}

	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		overlapped,
	)
}
