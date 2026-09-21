package transport

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
)

// StdioClient reads JSON-RPC lines from os.Stdin and writes to os.Stdout.
type StdioClient struct{}

func (StdioClient) Run(ctx context.Context, onMessage func([]byte, func([]byte, bool) error) func()) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	respond := func(b []byte, _ bool) error {
		os.Stdout.Write(b)
		_, err := os.Stdout.Write([]byte{'\n'})
		return err
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		onMessage(cp, respond)
	}
	return scanner.Err()
}

func (StdioClient) Broadcast(raw []byte) error {
	os.Stdout.Write(raw)
	_, err := os.Stdout.Write([]byte{'\n'})
	return err
}

func (StdioClient) Close() error { return nil }

// StdioUpstream spawns a subprocess and bridges its stdin/stdout.
type StdioUpstream struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.Reader
}

func NewStdioUpstream(command []string) (*StdioUpstream, error) {
	if len(command) == 0 {
		return nil, io.ErrShortWrite
	}
	cmd := exec.Command(command[0], command[1:]...)
	hideWindow(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &StdioUpstream{cmd: cmd, in: in, out: out}, nil
}

func (u *StdioUpstream) Run(ctx context.Context, onMessage func([]byte)) error {
	scanner := bufio.NewScanner(u.out)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	// Scan in a goroutine so the read loop can also honour ctx cancellation
	// (e.g. on shutdown), instead of blocking on scanner.Scan() forever.
	lines := make(chan []byte, 16)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			cp := make([]byte, len(line))
			copy(cp, line)
			lines <- cp
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return scanner.Err()
			}
			onMessage(line)
		}
	}
}

func (u *StdioUpstream) Write(raw []byte) error {
	if _, err := u.in.Write(raw); err != nil {
		return err
	}
	_, err := u.in.Write([]byte{'\n'})
	return err
}

func (u *StdioUpstream) Close() error {
	_ = u.in.Close()
	_ = u.cmd.Process.Kill()
	return u.cmd.Wait()
}
