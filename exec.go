package main

import (
	"bytes"
	"os"
	"os/exec"

	"github.com/pkg/errors"
)

// capture executes the command specified by args and returns its stdout. If
// the process exits with a failing exit code, capture instead returns an error
// which includes the process's stderr.
func capture(args ...string) (string, error) {
	var cmd *exec.Cmd
	if len(args) == 0 {
		panic("capture called with no arguments")
	} else if len(args) == 1 {
		cmd = exec.Command(args[0])
	} else {
		cmd = exec.Command(args[0], args[1:]...)
	}
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			err = errors.Errorf("%s: %s", err, exitErr.Stderr)
		}
		return "", err
	}
	return string(bytes.TrimSpace(out)), err
}

// spawn executes the command specified by args. The subprocess inherits the
// current processes's stdin, stdout, and stderr streams. If the process exits
// with a failing exit code, run returns a generic "process exited with
// status..." error, as the process has likely written an error message to
// stderr.
func spawn(args ...string) error {
	var cmd *exec.Cmd
	if len(args) == 0 {
		panic("spawn called with no arguments")
	} else if len(args) == 1 {
		cmd = exec.Command(args[0])
	} else {
		cmd = exec.Command(args[0], args[1:]...)
	}
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return errors.Wrapf(cmd.Run(), "spawned %s:", args)
}
