//go:build !windows

package hostbroker

import "os"

func replaceFile(source, target string) error { return os.Rename(source, target) }
