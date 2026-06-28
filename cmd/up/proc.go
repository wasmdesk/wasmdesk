// Copyright (c) 2026, the wasmdesk/wasmdesk authors. All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Runner abstracts the two things the orchestrator needs to do with external
// commands: run-and-wait (Run) and start-without-waiting (Spawn). Tests inject
// a fake; production uses execRunner which shells out via os/exec.
type Runner interface {
	// Run executes cmd in dir with extra env vars and waits for it to exit.
	// Returns an error if the command exits non-zero.
	Run(ctx context.Context, dir string, env []string, name string, args ...string) error
	// Spawn starts cmd and returns a Proc that pipes its stdout/stderr.
	// The Proc owns Kill+Wait semantics; Supervisor drives it.
	Spawn(ctx context.Context, dir string, env []string, name string, args ...string) (Proc, error)
}

// Proc is the surface the supervisor uses to manage one running child.
//
// We hide *exec.Cmd behind an interface so unit tests can drive a fake child
// that "exits" when its context is cancelled, without actually forking.
type Proc interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Signal(sig os.Signal) error
	Wait() error
}

// ---- production: os/exec-backed Runner ----

// execRunner is the os/exec-backed implementation of Runner. stdout/stderr are
// captured from Run() calls so build/registry phases get their output streamed
// onto the orchestrator's terminal (rather than vanishing into a buffer).
type execRunner struct {
	stdout, stderr io.Writer
}

// NewExecRunner builds the production Runner.
func NewExecRunner(stdout, stderr io.Writer) Runner {
	return &execRunner{stdout: stdout, stderr: stderr}
}

func (r *execRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = mergeEnv(os.Environ(), env)
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	return cmd.Run()
}

// Pipe seams: overridable so the rare StdoutPipe/StderrPipe error branches are
// reachable from tests (the real methods only fail after Start, so without a
// seam we cannot drive them).
var (
	stdoutPipeFn = (*exec.Cmd).StdoutPipe
	stderrPipeFn = (*exec.Cmd).StderrPipe
)

func (r *execRunner) Spawn(ctx context.Context, dir string, env []string, name string, args ...string) (Proc, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = mergeEnv(os.Environ(), env)
	stdout, err := stdoutPipeFn(cmd)
	if err != nil {
		return nil, err
	}
	stderr, err := stderrPipeFn(cmd)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

type execProc struct {
	cmd            *exec.Cmd
	stdout, stderr io.ReadCloser
}

func (p *execProc) Stdout() io.Reader { return p.stdout }
func (p *execProc) Stderr() io.Reader { return p.stderr }

func (p *execProc) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return fmt.Errorf("no process")
	}
	return p.cmd.Process.Signal(sig)
}

func (p *execProc) Wait() error { return p.cmd.Wait() }

// mergeEnv returns base with extra entries appended; extra wins on duplicate
// keys because Go's os/exec semantics use the last entry.
func mergeEnv(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

// ---- supervisor ----

// Supervisor owns the spawned children: it tails their output, broadcasts
// SIGTERM on shutdown, and reaps them with a hard SIGKILL fallback.
type Supervisor struct {
	stdout, stderr io.Writer

	mu        sync.Mutex
	children  []*child
	doneCh    chan struct{}
	closeOnce sync.Once
}

type child struct {
	name   string
	color  string
	proc   Proc
	doneCh chan error
}

// NewSupervisor returns an empty supervisor that will write its prefixed logs
// to the given writers. The writers are wrapped in a syncWriter so multiple
// tail goroutines can hammer the same underlying *bytes.Buffer or *os.File
// without racing.
func NewSupervisor(stdout, stderr io.Writer) *Supervisor {
	return &Supervisor{
		stdout: &syncWriter{w: stdout},
		stderr: &syncWriter{w: stderr},
		doneCh: make(chan struct{}, 1),
	}
}

// syncWriter serialises Writes on the wrapped writer. The whole orchestrator
// uses a handful of small Fprintf calls -- a sync.Mutex is plenty.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	if s.w == nil {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Start spawns the named command via r.Spawn and starts the prefix-tagged tail
// goroutines. The child is added to the supervisor's list.
func (s *Supervisor) Start(ctx context.Context, r Runner, name, color, dir, bin string, args, env []string) error {
	p, err := r.Spawn(ctx, dir, env, bin, args...)
	if err != nil {
		return err
	}
	c := &child{name: name, color: color, proc: p, doneCh: make(chan error, 1)}
	s.mu.Lock()
	s.children = append(s.children, c)
	s.mu.Unlock()
	go s.tail(c, p.Stdout(), false)
	go s.tail(c, p.Stderr(), true)
	go func() {
		err := p.Wait()
		c.doneCh <- err
		s.closeOnce.Do(func() {
			// Wake WaitAny() on the FIRST child exit only.
			close(s.doneCh)
		})
	}()
	return nil
}

// tail copies one of the child's streams to the supervisor's stdout, prefixing
// every line with the child's coloured "[name] ".
func (s *Supervisor) tail(c *child, r io.Reader, isStderr bool) {
	if r == nil {
		return
	}
	out := s.stdout
	if isStderr {
		out = s.stderr
	}
	br := bufio.NewScanner(r)
	br.Buffer(make([]byte, 64*1024), 1024*1024)
	for br.Scan() {
		fmt.Fprintf(out, "%s[%s]\033[0m %s\n", c.color, c.name, br.Text())
	}
}

// WaitAny returns a channel that closes the first time any child exits. Used
// by the orchestrator's main select so we tear the rest down if one dies.
func (s *Supervisor) WaitAny() <-chan struct{} { return s.doneCh }

// StopAll sends SIGTERM to every child, waits up to grace, then SIGKILLs the
// stragglers. Returns nil on clean shutdown.
func (s *Supervisor) StopAll(ctx context.Context, grace time.Duration) error {
	s.mu.Lock()
	cs := make([]*child, len(s.children))
	copy(cs, s.children)
	s.mu.Unlock()

	for _, c := range cs {
		_ = c.proc.Signal(syscall.SIGTERM)
	}
	// Wait for all of them (or grace expires).
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	pending := len(cs)
	for pending > 0 {
		select {
		case <-deadline.C:
			for _, c := range cs {
				_ = c.proc.Signal(syscall.SIGKILL)
			}
			return nil
		case <-firstDone(cs):
			pending--
		}
	}
	return nil
}

// firstDone returns a channel that fires once any child's doneCh receives.
// It is implemented as a fresh goroutine + channel so successive StopAll
// iterations each get a one-shot signal without draining all the children's
// doneCh channels.
func firstDone(cs []*child) <-chan struct{} {
	ch := make(chan struct{}, 1)
	go func() {
		for _, c := range cs {
			select {
			case <-c.doneCh:
				ch <- struct{}{}
				return
			default:
			}
		}
		// None ready synchronously -- wait on any.
		cases := make([]chan error, len(cs))
		for i, c := range cs {
			cases[i] = c.doneCh
		}
		// Fan-in.
		done := make(chan struct{}, 1)
		var once sync.Once
		for _, cc := range cases {
			cc := cc
			go func() {
				<-cc
				once.Do(func() { close(done) })
			}()
		}
		<-done
		ch <- struct{}{}
	}()
	return ch
}
