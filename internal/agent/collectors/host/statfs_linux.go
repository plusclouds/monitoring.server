//go:build linux

package host

import "syscall"

// statfs returns the size and used bytes of the filesystem at path.
func statfs(path string) (total, used float64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	bs := float64(s.Bsize)
	total = float64(s.Blocks) * bs
	used = (float64(s.Blocks) - float64(s.Bfree)) * bs
	return total, used, nil
}
