package server

import (
	"context"
	"sync"
)

// A capture owns at most two SSH/stdio transports across its entire fleet.
// Per-host multiplexers route many process nonces over one connection. Idle
// transports are joined before replacement; no dial/retry occurs implicitly.
type ArinShadowRPCPipePool struct {
	ctx           context.Context
	mu            sync.Mutex
	changed       chan struct{}
	closed        bool
	clock         uint64
	entries       map[string]*arinShadowPipeEntry
	commands      map[string][]string
	failed        map[string]bool
	started, peak int
}
type arinShadowPipeEntry struct {
	pipe   *ArinShadowRPCPipe
	active bool
	used   uint64
}

func NewArinShadowRPCPipePool(ctx context.Context, commands map[string][]string) (*ArinShadowRPCPipePool, error) {
	if ctx == nil || ctx.Err() != nil || len(commands) > 8 {
		return nil, ErrArinShadowInput
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, ErrArinShadowInput
	}
	p := &ArinShadowRPCPipePool{ctx: ctx, changed: make(chan struct{}), entries: map[string]*arinShadowPipeEntry{}, commands: map[string][]string{}, failed: map[string]bool{}}
	for name, argv := range commands {
		if name == "" || len(name) > 64 || len(argv) == 0 {
			return nil, ErrArinShadowInput
		}
		p.commands[name] = append([]string(nil), argv...)
	}
	return p, nil
}

func (p *ArinShadowRPCPipePool) wake() { close(p.changed); p.changed = make(chan struct{}) }

func (p *ArinShadowRPCPipePool) acquire(ctx context.Context, name string) (*arinShadowPipeEntry, error) {
	for {
		p.mu.Lock()
		if p.closed || ctx.Err() != nil || p.ctx.Err() != nil || p.commands[name] == nil || p.failed[name] || p.started >= 512 {
			p.mu.Unlock()
			return nil, ErrArinShadowInput
		}
		entry := p.entries[name]
		if entry == nil {
			if len(p.entries) == 2 {
				victim := ""
				var oldest uint64
				for key, value := range p.entries {
					if !value.active && (victim == "" || value.used < oldest) {
						victim = key
						oldest = value.used
					}
				}
				if victim != "" {
					p.entries[victim].pipe.Close()
					delete(p.entries, victim)
				}
			}
			if len(p.entries) < 2 {
				pipe, err := StartArinShadowRPCPipe(p.ctx, p.commands[name])
				if err != nil {
					p.failed[name] = true
					p.mu.Unlock()
					return nil, err
				}
				entry = &arinShadowPipeEntry{pipe: pipe}
				p.entries[name] = entry
				p.started++
				if len(p.entries) > p.peak {
					p.peak = len(p.entries)
				}
			}
		}
		if entry != nil && !entry.active {
			entry.active = true
			p.clock++
			entry.used = p.clock
			p.mu.Unlock()
			return entry, nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ErrArinShadowInput
		case <-p.ctx.Done():
			return nil, ErrArinShadowInput
		}
	}
}

func (p *ArinShadowRPCPipePool) RoundTrip(name string) ArinShadowRPCRoundTrip {
	return func(ctx context.Context, request []byte) ([]byte, error) {
		entry, err := p.acquire(ctx, name)
		if err != nil {
			return nil, err
		}
		defer func() {
			p.mu.Lock()
			entry.active = false
			p.clock++
			entry.used = p.clock
			p.wake()
			p.mu.Unlock()
		}()
		reply, err := entry.pipe.RoundTrip(ctx, request)
		if err != nil {
			p.mu.Lock()
			p.failed[name] = true
			p.mu.Unlock()
		}
		return reply, err
	}
}

func (p *ArinShadowRPCPipePool) Counts() (started, peak int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started, p.peak
}

func (p *ArinShadowRPCPipePool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.wake()
	entries := p.entries
	p.entries = nil
	p.mu.Unlock()
	for _, entry := range entries {
		entry.pipe.Close()
	}
}
