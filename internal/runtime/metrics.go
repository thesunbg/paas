package runtime

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "github.com/containerd/cgroups/v3/cgroup1/stats"
	v2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/thesunbg/paas/internal/proto"
)

func (r *Runtime) collectMetrics(ctx context.Context, id string) {
	ctr, err := r.client.LoadContainer(ctx, id)
	if err != nil {
		return
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		return
	}
	mt, err := task.Metrics(ctx)
	if err != nil {
		return
	}
	data, err := typeurl.UnmarshalAny(mt.Data)
	if err != nil {
		return
	}
	var memBytes, cpuNs uint64
	switch s := data.(type) {
	case *v1.Metrics:
		if s.Memory != nil && s.Memory.Usage != nil {
			memBytes = s.Memory.Usage.Usage
			if s.Memory.TotalInactiveFile < memBytes {
				memBytes -= s.Memory.TotalInactiveFile
			}
		}
		if s.CPU != nil && s.CPU.Usage != nil {
			cpuNs = s.CPU.Usage.Total
		}
	case *v2.Metrics:
		if s.Memory != nil {
			memBytes = s.Memory.Usage
			if s.Memory.InactiveFile < memBytes {
				memBytes -= s.Memory.InactiveFile
			}
		}
		if s.CPU != nil {
			cpuNs = s.CPU.UsageUsec * 1000
		}
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.containers[id]
	if m == nil {
		return
	}
	m.memMB = int(memBytes >> 20)
	if !m.lastCPUAt.IsZero() && cpuNs >= m.lastCPU {
		elapsed := now.Sub(m.lastCPUAt).Nanoseconds()
		if elapsed > 0 {
			m.cpuMillis = int((cpuNs - m.lastCPU) * 1000 / uint64(elapsed))
		}
	}
	m.lastCPU = cpuNs
	m.lastCPUAt = now
}

// Logs returns the last n lines of a container's output.
func (r *Runtime) Logs(id string, n int) ([]string, error) {
	return tailFile(r.LogPath(id), n)
}

func tailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const maxRead = 1 << 20
	off := st.Size() - maxRead
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// Follow streams new log lines to fn until ctx is cancelled.
func (r *Runtime) Follow(ctx context.Context, id string, fn func(line string)) error {
	f, err := os.Open(r.LogPath(id))
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	reader := bufio.NewReader(f)
	var partial string
	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			fn(strings.TrimRight(partial+line, "\n"))
			partial = ""
			continue
		}
		partial += line
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
		// Handle truncation / rotation.
		if st, err := os.Stat(r.LogPath(id)); err == nil {
			if pos, _ := f.Seek(0, io.SeekCurrent); st.Size() < pos {
				_, _ = f.Seek(0, io.SeekStart)
				reader.Reset(f)
			}
		}
	}
}

// Exec runs a command inside a running container.
func (r *Runtime) Exec(ctx context.Context, req proto.ExecRequest) proto.ExecReply {
	cctx := r.ctx(ctx)
	timeout := time.Duration(req.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cctx, cancel := context.WithTimeout(cctx, timeout)
	defer cancel()

	ctr, err := r.client.LoadContainer(cctx, req.ContainerID)
	if err != nil {
		return proto.ExecReply{Error: err.Error()}
	}
	task, err := ctr.Task(cctx, nil)
	if err != nil {
		return proto.ExecReply{Error: "container is not running"}
	}
	spec, err := ctr.Spec(cctx)
	if err != nil {
		return proto.ExecReply{Error: err.Error()}
	}
	pspec := *spec.Process
	pspec.Args = req.Cmd
	pspec.Terminal = false
	pspec.Env = append(append([]string{}, spec.Process.Env...), req.Env...)
	pspec.Capabilities = spec.Process.Capabilities
	_ = specs.Process{}

	var stdin io.Reader = strings.NewReader(req.Stdin)
	if req.StdinFile != "" {
		f, err := os.Open(req.StdinFile)
		if err != nil {
			return proto.ExecReply{Error: err.Error()}
		}
		defer f.Close()
		stdin = f
	}
	var stdoutBuf bytes.Buffer
	var stdout io.Writer = &limitedBuffer{buf: &stdoutBuf, max: 1 << 20}
	var outFile *os.File
	if req.StdoutFile != "" {
		outFile, err = os.OpenFile(req.StdoutFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return proto.ExecReply{Error: err.Error()}
		}
		defer outFile.Close()
		stdout = outFile
	}
	var stderrBuf bytes.Buffer
	execID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	eof := &eofReader{r: stdin, done: make(chan struct{})}
	proc, err := task.Exec(cctx, execID, &pspec, cio.NewCreator(cio.WithStreams(eof, stdout, &limitedBuffer{buf: &stderrBuf, max: 1 << 20})))
	if err != nil {
		return proto.ExecReply{Error: err.Error()}
	}
	go func() {
		// Close the process stdin once everything was sent, so tools like psql see EOF.
		select {
		case <-eof.done:
			_ = proc.CloseIO(cctx, client.WithStdinCloser)
		case <-cctx.Done():
		}
	}()
	defer proc.Delete(context.WithoutCancel(cctx))
	statusC, err := proc.Wait(cctx)
	if err != nil {
		return proto.ExecReply{Error: err.Error()}
	}
	if err := proc.Start(cctx); err != nil {
		return proto.ExecReply{Error: err.Error()}
	}
	var st interface{ ExitCode() uint32 }
	select {
	case s := <-statusC:
		st = s
	case <-cctx.Done():
		_ = proc.Kill(context.Background(), syscall.SIGKILL)
		return proto.ExecReply{Error: "exec timed out"}
	}
	proc.IO().Wait()
	rep := proto.ExecReply{ExitCode: int(st.ExitCode()), Stdout: stdoutBuf.String(), Stderr: stderrBuf.String()}
	if outFile != nil {
		if fi, err := outFile.Stat(); err == nil {
			rep.StdoutBytes = fi.Size()
		}
	}
	return rep
}

type eofReader struct {
	r    io.Reader
	once sync.Once
	done chan struct{}
}

func (e *eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		e.once.Do(func() { close(e.done) })
	}
	return n, err
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}
