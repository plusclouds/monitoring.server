//go:build unix

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Process is the application `agent run` wraps: started first, signals
// forwarded to it, orphans reaped when the agent is PID 1, exit code passed
// on. The agent is a supervisor, not a restarter: the container runtime owns
// restart policy.
type Process struct {
	args    []string
	pid     int
	started time.Time

	mu     sync.Mutex
	exited bool
	code   int
	signal syscall.Signal
	done   chan struct{}
}

// StartProcess starts args[0] with the agent's stdio and environment.
func StartProcess(args []string) (*Process, error) {
	if len(args) == 0 {
		return nil, errors.New("no command to run")
	}
	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...) //nolint:gosec // the image's own entrypoint
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// SIGCHLD must be caught before the child can exit, or its exit is missed.
	chld := make(chan os.Signal, 16)
	signal.Notify(chld, syscall.SIGCHLD)
	if err := cmd.Start(); err != nil {
		signal.Stop(chld)
		return nil, err
	}
	p := &Process{args: args, pid: cmd.Process.Pid, started: time.Now(), done: make(chan struct{})}

	fwd := make(chan os.Signal, 16)
	signal.Notify(fwd, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for s := range fwd {
			if sig, ok := s.(syscall.Signal); ok {
				_ = syscall.Kill(p.pid, sig)
			}
		}
	}()
	go p.reap(chld, fwd)
	return p, nil
}

// reap waits for the child on SIGCHLD. As PID 1 it also collects every
// orphan that is re-parented to the agent, so zombies do not pile up.
func (p *Process) reap(chld, fwd chan os.Signal) {
	pid1 := os.Getpid() == 1
	target := p.pid
	if pid1 {
		target = -1
	}
	tick := time.NewTicker(time.Second) // a SIGCHLD can coalesce; poll as a backstop
	defer tick.Stop()
	for {
		for {
			var ws syscall.WaitStatus
			got, err := syscall.Wait4(target, &ws, syscall.WNOHANG, nil)
			if err == syscall.EINTR {
				continue
			}
			if err != nil || got <= 0 {
				break
			}
			if got == p.pid {
				p.mu.Lock()
				p.exited = true
				if ws.Signaled() {
					p.signal, p.code = ws.Signal(), 128+int(ws.Signal())
				} else {
					p.code = ws.ExitStatus()
				}
				p.mu.Unlock()
				signal.Stop(chld)
				signal.Stop(fwd)
				close(fwd)
				close(p.done)
				return
			}
		}
		select {
		case <-chld:
		case <-tick.C:
		}
	}
}

// Done is closed when the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// ExitCode is the shell convention: the code, or 128+signal.
func (p *Process) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

// Object is the process as a reported object.
func (p *Process) Object() Object {
	p.mu.Lock()
	defer p.mu.Unlock()
	o := Object{Key: "process:main", Kind: "process", Name: filepath.Base(p.args[0]),
		Labels:  map[string]string{"command": p.args[0], "pid": fmt.Sprint(p.pid)},
		Metrics: map[string]float64{"up": 1, "process_count": 1, "uptime_seconds": time.Since(p.started).Seconds()}}
	if p.exited {
		o.Metrics["up"], o.Metrics["process_count"] = 0, 0
		o.Status, o.Output = Warning, "exited with code 0"
		if p.code != 0 {
			o.Status = Critical
			o.Output = fmt.Sprintf("exited with code %d", p.code)
			if p.signal != 0 {
				o.Output = fmt.Sprintf("killed by signal %d (%s)", int(p.signal), p.signal)
			}
		}
	}
	return o
}
