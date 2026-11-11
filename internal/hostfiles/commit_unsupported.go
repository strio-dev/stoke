//go:build !linux

package hostfiles

import "errors"

// Commit requires Linux's atomic nonreplacement rename operation.
func Commit(_, _ string) error { return errors.New("host-file atomic commit requires Linux") }
