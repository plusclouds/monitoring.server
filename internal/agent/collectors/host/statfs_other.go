//go:build !linux

package host

import "errors"

func statfs(string) (total, used float64, err error) { return 0, 0, errors.New("statfs: linux only") }
