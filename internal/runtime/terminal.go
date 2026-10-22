package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Terminal is an interactive process with a pseudo-terminal inside a container.
type Terminal struct {
	proc  client.Process
	stdin *io.PipeWriter
	done  chan struct{}
	code  int
}

// defaultShell prefers bash and falls back to sh.
var defaultShell = []string{"/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash -l; fi; exec sh -l"}

// OpenTerminal starts cmd (default: a login shell) with a terminal of cols×rows in a running
// container. Output, stdout and stderr merged as a terminal does, is written to out.
func (r *Runtime) OpenTerminal(ctx context.Context, containerID string, cmd []string, cols, rows int, out io.Writer) (*Terminal, error) {
	cctx := r.ctx(context.WithoutCancel(ctx))
	ctr, err := r.client.LoadContainer(cctx, containerID)
	if err != nil {
		return nil, err
	}
	task, err := ctr.Task(cctx, nil)
	if err != nil {
		return nil, errors.New("container is not running")
	}
	spec, err := ctr.Spec(cctx)
	if err != nil {
		return nil, err
	}
	if len(cmd) == 0 {
		cmd = defaultShell
	}
	pspec := *spec.Process
	pspec.Args = cmd
	pspec.Terminal = true
	pspec.Env = append(append([]string{}, spec.Process.Env...), "TERM=xterm-256color", "COLORTERM=truecolor")
	if cols > 0 && rows > 0 {
		pspec.ConsoleSize = &specs.Box{Width: uint(cols), Height: uint(rows)}
	}
	inR, inW := io.Pipe()
	id := fmt.Sprintf("term-%d", time.Now().UnixNano())
	proc, err := task.Exec(cctx, id, &pspec, cio.NewCreator(cio.WithStreams(inR, out, nil), cio.WithTerminal))
	if err != nil {
		return nil, err
	}
	statusC, err := proc.Wait(cctx)
	if err != nil {
		_, _ = proc.Delete(cctx)
		return nil, err
	}
	if err := proc.Start(cctx); err != nil {
		_, _ = proc.Delete(cctx)
		return nil, err
	}
	t := &Terminal{proc: proc, stdin: inW, done: make(chan struct{}), code: -1}
	if cols > 0 && rows > 0 {
		_ = proc.Resize(cctx, uint32(cols), uint32(rows))
	}
	go func() {
		st := <-statusC
		t.code = int(st.ExitCode())
		_ = inW.Close()
		proc.IO().Wait()
		_, _ = proc.Delete(context.Background())
		close(t.done)
	}()
	return t, nil
}

// Write sends keystrokes to the process.
func (t *Terminal) Write(p []byte) (int, error) { return t.stdin.Write(p) }

// Resize changes the terminal size.
func (t *Terminal) Resize(ctx context.Context, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return t.proc.Resize(ctx, uint32(cols), uint32(rows))
}

// Close ends the process (SIGHUP, then SIGKILL if it lingers) and waits for it.
func (t *Terminal) Close() {
	select {
	case <-t.done:
		return
	default:
	}
	_ = t.proc.Kill(context.Background(), syscall.SIGHUP)
	select {
	case <-t.done:
	case <-time.After(3 * time.Second):
		_ = t.proc.Kill(context.Background(), syscall.SIGKILL)
		<-t.done
	}
}

// Done is closed when the process has exited; ExitCode is valid then.
func (t *Terminal) Done() <-chan struct{} { return t.done }
func (t *Terminal) ExitCode() int         { return t.code }
