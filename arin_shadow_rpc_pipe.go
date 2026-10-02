package server

import (
	"context"
	"io"
	"os/exec"
	"time"
)

// One reviewed operator command owns a persistent framed pipe. Auth material
// stays in stdin packets, never argv. No shell, retry, background process or
// inherited stdout/stderr logger is used. The parent capture bounds its lifetime.
type ArinShadowRPCPipe struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  io.ReadCloser
	cancel  context.CancelFunc
	done    chan struct{}
	permit  chan struct{}
	closed  bool
}

func StartArinShadowRPCPipe(ctx context.Context, argv []string) (*ArinShadowRPCPipe, error) {
	if ctx == nil || ctx.Err() != nil || len(argv) == 0 || len(argv) > 64 || argv[0] == "" || argv[0][0] != '/' {
		return nil, ErrArinShadowInput
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, ErrArinShadowInput
	}
	length := 0
	for _, arg := range argv {
		length += len(arg)
	}
	if length > 16384 {
		return nil, ErrArinShadowInput
	}
	owned, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(owned, argv[0], argv[1:]...)
	// Stderr is intentionally discarded: credentials, peer IDs or SSH errors
	// cannot enter the aggregate artifact. A failure remains a finite unknown.
	command.Stderr = nil // /dev/null; no copier goroutine can outlive a child.
	command.WaitDelay = time.Second
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, ErrArinShadowInput
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		cancel()
		return nil, ErrArinShadowInput
	}
	if command.Start() != nil {
		input.Close()
		output.Close()
		cancel()
		return nil, ErrArinShadowInput
	}
	pipe := &ArinShadowRPCPipe{command: command, input: input, output: output, cancel: cancel, done: make(chan struct{}), permit: make(chan struct{}, 1)}
	go func() { command.Wait(); close(pipe.done) }()
	return pipe, nil
}

func (p *ArinShadowRPCPipe) RoundTrip(ctx context.Context, request []byte) ([]byte, error) {
	select {
	case p.permit <- struct{}{}:
		defer func() { <-p.permit }()
	case <-ctx.Done():
		return nil, ErrArinShadowInput
	}
	if p.closed || ctx.Err() != nil {
		return nil, ErrArinShadowInput
	}
	stop := context.AfterFunc(ctx, p.cancel)
	defer stop()
	if WriteArinShadowRPCFrame(p.input, request, ArinShadowRPCRequestLimit) != nil {
		p.closed = true
		p.cancel()
		return nil, ErrArinShadowInput
	}
	reply, err := ReadArinShadowRPCFrame(p.output, ArinShadowRPCResponseLimit)
	if err != nil || ctx.Err() != nil {
		p.closed = true
		p.cancel()
		return nil, ErrArinShadowInput
	}
	return reply, nil
}

func (p *ArinShadowRPCPipe) Close() {
	if p == nil {
		return
	}
	p.cancel()
	p.input.Close()
	p.output.Close()
	<-p.done
}
