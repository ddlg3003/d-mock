// Package codec runs the commands that encode or decode a payload (encrypt, sign, base64…):
// the payload on stdin, the result on stdout. The command is an argv list run without a
// shell, so a payload can never be read as a command.
package codec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// MaxOutput bounds what a codec command may write (a runaway command can't exhaust memory).
const MaxOutput = 16 << 20

// Timeout is how long one encode or decode may take.
const Timeout = 10 * time.Second

// Run pipes in through argv (run from dir) and returns its stdout.
func Run(ctx context.Context, argv []string, dir string, in []byte) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	cctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(in)
	var out limited
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s: took longer than %s", argv[0], Timeout)
	}
	if out.over {
		return nil, fmt.Errorf("%s: wrote more than %d bytes", argv[0], MaxOutput)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

type limited struct {
	bytes.Buffer
	over bool
}

func (l *limited) Write(p []byte) (int, error) {
	if l.Len()+len(p) > MaxOutput {
		l.over = true
		return 0, io.ErrShortWrite
	}
	return l.Buffer.Write(p)
}
