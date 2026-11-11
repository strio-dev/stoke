//go:build !linux

package runner

import (
	"context"
	"errors"
	"os/exec"
)

func runHostProcess(context.Context, *exec.Cmd) error {
	return errors.New("host steps currently require Linux process supervision")
}
