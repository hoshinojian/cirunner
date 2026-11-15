package githubapi

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

// runGH 跑 gh 取 stdout（--jq 已裁剪输出）；stderr 透传。
func runGH(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		if errBuf.Len() > 0 {
			return out, &ghError{err: err, stderr: errBuf.String()}
		}
		return out, err
	}
	return []byte(strings.TrimSpace(string(out))), nil
}

type ghError struct {
	err    error
	stderr string
}

func (e *ghError) Error() string { return e.err.Error() + ": " + e.stderr }
func (e *ghError) Unwrap() error { return e.err }
