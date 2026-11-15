package vpsctlapi

import (
	"context"
	"os/exec"
)

// execOutput 跑子进程取 stdout；stderr 透传给用户（vpsctl 的人读信息走 stderr）。
func execOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderrBuf safeBuffer
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil {
		if stderrBuf.Len() > 0 {
			return out, &cmdError{err: err, stderr: stderrBuf.String()}
		}
		return out, err
	}
	return out, nil
}

type safeBuffer struct{ b []byte }

func (s *safeBuffer) Write(p []byte) (int, error) { s.b = append(s.b, p...); return len(p), nil }
func (s *safeBuffer) String() string              { return string(s.b) }
func (s *safeBuffer) Len() int                    { return len(s.b) }

type cmdError struct {
	err    error
	stderr string
}

func (e *cmdError) Error() string { return e.err.Error() + ": " + e.stderr }
func (e *cmdError) Unwrap() error { return e.err }
