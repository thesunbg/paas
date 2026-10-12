// Package runtime runs workloads directly on containerd (no Docker daemon),
// using crun by default and gVisor for sandboxed services.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
)

const (
	labelInstance = "paas.instance"
	labelService  = "paas.service"
	labelManaged  = "paas.managed"
)

// Runtime manages containers on this node.
type Runtime struct {
	client    *client.Client
	ns        string
	dataDir   string
	ociBinary string // crun | runc
	net       *network.Manager
	cgroupV2  bool
	negOOM    bool // kernel lets us lower oom_score_adj (protect databases)

	mu         sync.Mutex
	containers map[string]*managed

	// OnChange is called when a container exits or comes back (for prompt status reports).
	OnChange func()
}

func (r *Runtime) changed() {
	if r.OnChange != nil {
		go r.OnChange()
	}
}

type managed struct {
	Req       proto.RunRequest `json:"req"`
	IP        string           `json:"ip"`
	NetNS     string           `json:"netns"`
	Restarts  int              `json:"restarts"`
	Stopped   bool             `json:"stopped"`
	Evicted   bool             `json:"evicted"`
	ExitCode  int              `json:"exit_code"`
	lastCPU   uint64
	lastCPUAt time.Time
	cpuMillis int
	memMB     int
	running   bool
	cancel    context.CancelFunc
	exited    chan struct{} // closed when a run-once container exits
}

func New(ctx context.Context, sock, ns, dataDir, ociRuntime string, nm *network.Manager) (*Runtime, error) {
	c, err := client.New(sock, client.WithDefaultNamespace(ns), client.WithTimeout(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("connect containerd at %s: %w", sock, err)
	}
	if ociRuntime == "crun" {
		if _, err := exec.LookPath("crun"); err != nil {
			slog.Warn("crun not found, falling back to runc")
			ociRuntime = "runc"
		}
	}
	_, statErr := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	if _, err := os.Stat("/sys/fs/cgroup/unified"); err == nil && statErr != nil && ociRuntime == "crun" {
		// crun does not support the cgroup v1/v2 hybrid layout.
		slog.Warn("hybrid cgroup layout detected, using runc instead of crun")
		ociRuntime = "runc"
	}
	r := &Runtime{
		client:     c,
		ns:         ns,
		dataDir:    dataDir,
		ociBinary:  ociRuntime,
		net:        nm,
		cgroupV2:   statErr == nil,
		negOOM:     canLowerOOMScore(),
		containers: map[string]*managed{},
	}
	for _, d := range []string{"containers", "volumes", "netns"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Runtime) Close() error { return r.client.Close() }

func (r *Runtime) Client() *client.Client { return r.client }

func (r *Runtime) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, r.ns)
}

// Capabilities reports optional runtime features of this node.
func (r *Runtime) Capabilities() map[string]string {
	caps := map[string]string{"oci_runtime": r.ociBinary, "cgroup": "v1"}
	if r.cgroupV2 {
		caps["cgroup"] = "v2"
	}
	if _, err := exec.LookPath("containerd-shim-runsc-v1"); err == nil {
		caps["runtime.sandboxed"] = "true"
	}
	if _, err := os.Stat("/dev/kvm"); err == nil {
		caps["kvm"] = "true"
	}
	return caps
}

func (r *Runtime) containerDir(id string) string { return filepath.Join(r.dataDir, "containers", id) }

// LogPath is where container stdout/stderr is written.
func (r *Runtime) LogPath(id string) string { return filepath.Join(r.containerDir(id), "output.log") }

func (r *Runtime) saveState(id string, m *managed) {
	data, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(filepath.Join(r.containerDir(id), "state.json"), data, 0o600)
}

// EnsureImage makes sure an image exists locally, pulling it when allowed.
// plainHost names a registry (host:port) reached over plain HTTP (the internal registry).
func (r *Runtime) EnsureImage(ctx context.Context, ref string, pull bool, plainHost string) (client.Image, error) {
	ctx = r.ctx(ctx)
	img, err := r.client.GetImage(ctx, ref)
	if err == nil {
		return img, nil
	}
	if !errdefs.IsNotFound(err) || !pull {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	slog.Info("pulling image", "ref", ref)
	return r.client.Pull(ctx, ref, client.WithPullUnpack, client.WithResolver(r.resolver(ctx, plainHost)))
}

func (r *Runtime) resolver(ctx context.Context, plainHost string) remotes.Resolver {
	if plainHost != "" && !strings.Contains(plainHost, "/") {
		return docker.NewResolver(docker.ResolverOptions{
			Hosts: func(host string) ([]docker.RegistryHost, error) {
				if host == plainHost {
					return docker.ConfigureDefaultRegistries(docker.WithPlainHTTP(docker.MatchAllHosts))(host)
				}
				return dockerconfig.ConfigureHosts(ctx, dockerconfig.HostOptions{HostDir: dockerconfig.HostDirFromRoot("/etc/containerd/certs.d")})(host)
			},
		})
	}
	return docker.NewResolver(docker.ResolverOptions{
		Hosts: dockerconfig.ConfigureHosts(ctx, dockerconfig.HostOptions{HostDir: dockerconfig.HostDirFromRoot("/etc/containerd/certs.d")}),
	})
}

// Push copies a local image to a registry reference.
func (r *Runtime) Push(ctx context.Context, src, target string, insecure bool) error {
	ctx = r.ctx(ctx)
	img, err := r.client.GetImage(ctx, src)
	if err != nil {
		return fmt.Errorf("image %s: %w", src, err)
	}
	plain := ""
	if insecure {
		plain = strings.SplitN(target, "/", 2)[0]
	}
	slog.Info("pushing image", "src", src, "target", target)
	return r.client.Push(ctx, target, img.Target(), client.WithResolver(r.resolver(ctx, plain)))
}

// NormalizeRef expands short image names (postgres:16) to fully-qualified references.
func NormalizeRef(ref string) string {
	first := strings.SplitN(ref, "/", 2)[0]
	hasRegistry := strings.Contains(ref, "/") && (strings.ContainsAny(first, ".:") || first == "localhost")
	if !hasRegistry {
		if !strings.Contains(ref, "/") {
			ref = "library/" + ref
		}
		ref = "docker.io/" + ref
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	if !strings.Contains(last, ":") && !strings.Contains(last, "@") {
		ref += ":latest"
	}
	return ref
}

// Run creates and starts a container, waits for it to pass its health check and returns its IP.
func (r *Runtime) Run(ctx context.Context, req proto.RunRequest) (string, error) {
	id := req.ContainerID
	if id == "" {
		return "", errors.New("container id required")
	}
	img, err := r.EnsureImage(ctx, req.Image, req.PullImage, req.PlainHTTPRegistry)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(r.containerDir(id), 0o755); err != nil {
		return "", err
	}
	m := &managed{Req: req}
	nsh, err := netns.NewNetNS(filepath.Join(r.dataDir, "netns"))
	if err != nil {
		return "", fmt.Errorf("create netns: %w", err)
	}
	m.NetNS = nsh.GetPath()
	// Release any address left behind by an earlier attempt with the same container id.
	_ = r.net.Detach(ctx, req.NetIndex, id, m.NetNS)
	ip, err := r.net.Attach(ctx, req.NetIndex, id, m.NetNS)
	if err != nil {
		_ = nsh.Remove()
		return "", err
	}
	m.IP = ip
	if err := r.writeNetFiles(id, req); err != nil {
		return "", err
	}

	cctx := r.ctx(ctx)
	if err := r.createContainer(cctx, img, m); err != nil {
		_ = r.net.Detach(ctx, req.NetIndex, id, m.NetNS)
		_ = nsh.Remove()
		return "", err
	}
	r.saveState(id, m)
	r.mu.Lock()
	r.containers[id] = m
	r.mu.Unlock()

	if req.Once {
		m.exited = make(chan struct{})
	}
	if err := r.startTask(cctx, id); err != nil {
		_ = r.Remove(ctx, id, 0)
		return "", err
	}
	if req.Once {
		return ip, nil
	}
	if err := r.waitHealthy(ctx, id, ip, req.Port, req.HealthCheck); err != nil {
		tail, _ := r.Logs(id, 30)
		_ = r.Remove(ctx, id, 0)
		return "", fmt.Errorf("%w\n--- last log lines ---\n%s", err, strings.Join(tail, "\n"))
	}
	return ip, nil
}

func (r *Runtime) writeNetFiles(id string, req proto.RunRequest) error {
	dir := r.containerDir(id)
	gw := r.net.Gateway(req.NetIndex)
	resolv := fmt.Sprintf("nameserver %s\noptions ndots:1 timeout:2\n", gw)
	if req.DNSSearch != "" {
		resolv = fmt.Sprintf("nameserver %s\nsearch %s\noptions ndots:1 timeout:2\n", gw, req.DNSSearch)
	}
	if err := os.WriteFile(filepath.Join(dir, "resolv.conf"), []byte(resolv), 0o644); err != nil {
		return err
	}
	hosts := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if req.Hostname != "" {
		hosts += "127.0.1.1\t" + req.Hostname + "\n"
	}
	return os.WriteFile(filepath.Join(dir, "hosts"), []byte(hosts), 0o644)
}

func (r *Runtime) runtimeFor(class string) (string, any) {
	if class == model.RuntimeSandboxed {
		if _, err := exec.LookPath("containerd-shim-runsc-v1"); err == nil {
			return "io.containerd.runsc.v1", nil
		}
		slog.Warn("sandboxed runtime requested but gVisor is not installed; using standard runtime")
	}
	return "io.containerd.runc.v2", &options.Options{BinaryName: r.ociBinary}
}

func (r *Runtime) createContainer(ctx context.Context, img client.Image, m *managed) error {
	req := m.Req
	dir := r.containerDir(req.ContainerID)
	env := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	mounts := []specs.Mount{
		{Destination: "/etc/resolv.conf", Type: "bind", Source: filepath.Join(dir, "resolv.conf"), Options: []string{"rbind", "ro"}},
		{Destination: "/etc/hosts", Type: "bind", Source: filepath.Join(dir, "hosts"), Options: []string{"rbind", "ro"}},
	}
	for _, v := range req.Volumes {
		host := filepath.Join(r.dataDir, "volumes", req.ServiceID, v.Name)
		if err := os.MkdirAll(host, 0o755); err != nil {
			return err
		}
		mounts = append(mounts, specs.Mount{Destination: v.Path, Type: "bind", Source: host, Options: []string{"rbind", "rw", "nosuid", "nodev"}})
	}

	specOpts := []oci.SpecOpts{
		oci.WithDefaultSpec(),
		oci.WithDefaultUnixDevices,
		oci.WithImageConfig(img),
		oci.WithEnv(env),
		oci.WithHostname(firstNonEmpty(req.Hostname, req.ContainerID[:12])),
		oci.WithMounts(mounts),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: m.NetNS}),
		oci.WithNoNewPrivileges,
		oci.WithDroppedCapabilities([]string{"CAP_NET_RAW", "CAP_MKNOD", "CAP_AUDIT_WRITE"}),
		oci.WithPidsLimit(4096),
		seccomp.WithDefaultProfile(),
		withResources(req.Resources, req.QoS, r.cgroupV2, r.negOOM),
	}
	if len(req.Command) > 0 {
		specOpts = append(specOpts, withCommand(img, req.Command))
	}
	rtName, rtOpts := r.runtimeFor(req.RuntimeClass)
	_, err := r.client.NewContainer(ctx, req.ContainerID,
		client.WithImage(img),
		client.WithNewSnapshot(req.ContainerID+"-snap", img),
		client.WithRuntime(rtName, rtOpts),
		client.WithContainerLabels(map[string]string{
			labelManaged:  "true",
			labelInstance: req.InstanceID,
			labelService:  req.ServiceID,
		}),
		client.WithNewSpec(specOpts...),
	)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	return nil
}

// withCommand overrides the image command. A single string is run through /bin/sh -c.
func withCommand(img client.Image, cmd []string) oci.SpecOpts {
	return func(ctx context.Context, c oci.Client, ctr *containers.Container, s *oci.Spec) error {
		if len(cmd) == 1 {
			s.Process.Args = []string{"/bin/sh", "-c", cmd[0]}
			return nil
		}
		s.Process.Args = cmd
		return nil
	}
}

// withResources applies memory/CPU limits and the QoS policy to the OCI spec.
func withResources(res model.Resources, qos string, cgroupV2, negOOM bool) oci.SpecOpts {
	return func(ctx context.Context, c oci.Client, ctr *containers.Container, s *oci.Spec) error {
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		if s.Linux.Resources == nil {
			s.Linux.Resources = &specs.LinuxResources{}
		}
		lr := s.Linux.Resources
		if res.MemoryLimitMB > 0 {
			limit := int64(res.MemoryLimitMB) << 20
			lr.Memory = &specs.LinuxMemory{Limit: &limit}
			if qos == model.QoSGuaranteed {
				// No swap for guaranteed workloads (databases).
				swap := limit
				lr.Memory.Swap = &swap
			}
		}
		if res.CPULimitMillis > 0 {
			period := uint64(100000)
			quota := int64(res.CPULimitMillis) * 100
			lr.CPU = &specs.LinuxCPU{Period: &period, Quota: &quota}
		}
		shares := uint64(1024)
		if res.CPURequestMillis > 0 {
			shares = uint64(res.CPURequestMillis) * 1024 / 1000
			if shares < 2 {
				shares = 2
			}
		}
		if lr.CPU == nil {
			lr.CPU = &specs.LinuxCPU{}
		}
		lr.CPU.Shares = &shares
		if cgroupV2 {
			if lr.Unified == nil {
				lr.Unified = map[string]string{}
			}
			if res.MemoryLimitMB > 0 && qos != model.QoSGuaranteed {
				// Soft limit: the kernel reclaims before hitting the hard limit.
				lr.Unified["memory.high"] = strconv.FormatInt(int64(res.MemoryLimitMB)*9/10<<20, 10)
			}
			if qos == model.QoSGuaranteed {
				lr.Unified["memory.swap.max"] = "0"
			}
		}
		adj := 500
		switch qos {
		case model.QoSGuaranteed:
			adj = -900
			if !negOOM {
				adj = 0
			}
		case model.QoSBestEffort:
			adj = 1000
		}
		s.Process.OOMScoreAdj = &adj
		return nil
	}
}

// canLowerOOMScore reports whether negative oom_score_adj values are permitted here.
func canLowerOOMScore() bool {
	cur, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		return false
	}
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("-1"), 0o644); err != nil {
		return false
	}
	_ = os.WriteFile("/proc/self/oom_score_adj", cur, 0o644)
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (r *Runtime) startTask(ctx context.Context, id string) error {
	ctr, err := r.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	if t, err := ctr.Task(ctx, nil); err == nil {
		_, _ = t.Delete(ctx, client.WithProcessKill)
	}
	logPath := r.LogPath(id)
	rotateLog(logPath)
	task, err := ctr.NewTask(ctx, cio.LogFile(logPath))
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	// Wait must outlive the caller's request context, otherwise cancelling it
	// reports a bogus exit (code 255) and triggers a restart.
	exitCh, err := task.Wait(r.ctx(context.Background()))
	if err != nil {
		_, _ = task.Delete(ctx)
		return err
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(ctx)
		return fmt.Errorf("start task: %w", err)
	}
	r.mu.Lock()
	m := r.containers[id]
	var watchCtx context.Context
	if m != nil {
		m.running = true
		m.Stopped = false
		var cancel context.CancelFunc
		watchCtx, cancel = context.WithCancel(context.Background())
		m.cancel = cancel
	}
	r.mu.Unlock()
	if m != nil {
		go r.watch(watchCtx, id, exitCh)
	}
	return nil
}

// watch restarts a container with exponential backoff when it exits unexpectedly.
func (r *Runtime) watch(ctx context.Context, id string, exitCh <-chan client.ExitStatus) {
	var st client.ExitStatus
	select {
	case <-ctx.Done():
		return
	case st = <-exitCh:
	}
	r.mu.Lock()
	m := r.containers[id]
	if m == nil {
		r.mu.Unlock()
		return
	}
	m.running = false
	m.ExitCode = int(st.ExitCode())
	if m.Req.Once {
		if m.exited != nil {
			close(m.exited)
			m.exited = nil
		}
		r.mu.Unlock()
		return
	}
	stopped := m.Stopped || m.Evicted
	m.Restarts++
	restarts := m.Restarts
	r.saveState(id, m)
	r.mu.Unlock()
	if stopped {
		return
	}
	r.changed()
	backoff := time.Duration(1<<min(restarts-1, 7)) * 250 * time.Millisecond
	slog.Warn("container exited, restarting", "id", id, "exit", st.ExitCode(), "backoff", backoff)
	select {
	case <-ctx.Done():
		return
	case <-time.After(backoff):
	}
	r.mu.Lock()
	m = r.containers[id]
	stopped = m == nil || m.Stopped || m.Evicted
	r.mu.Unlock()
	if stopped {
		return
	}
	for attempt := 1; ; attempt++ {
		err := r.startTask(r.ctx(context.Background()), id)
		if err == nil {
			r.changed()
			return
		}
		slog.Error("restart failed", "id", id, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(1<<min(attempt+restarts, 6)) * time.Second):
		}
		r.mu.Lock()
		m = r.containers[id]
		stopped = m == nil || m.Stopped || m.Evicted
		r.mu.Unlock()
		if stopped {
			return
		}
	}
}

func (r *Runtime) waitHealthy(ctx context.Context, id, ip string, port int, hc model.HealthCheck) error {
	timeout := time.Duration(hc.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	deadline := time.Now().Add(timeout)
	kind := hc.Type
	if kind == "" {
		kind = "tcp"
		if hc.Path != "" {
			kind = "http"
		}
	}
	if port == 0 || kind == "none" {
		// Workers: healthy if still running after a short grace period.
		time.Sleep(2 * time.Second)
		if !r.isRunning(id) {
			return fmt.Errorf("process exited during startup")
		}
		return nil
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	httpc := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	delay := 100 * time.Millisecond
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.Lock()
		m := r.containers[id]
		crashed := m != nil && !m.running && m.Restarts > 0
		r.mu.Unlock()
		if crashed {
			return fmt.Errorf("process exited during startup (exit code %d)", m.ExitCode)
		}
		ok := false
		if kind == "http" {
			resp, err := httpc.Get("http://" + addr + hc.Path)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				ok = resp.StatusCode < 500
			}
		} else {
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				conn.Close()
				ok = true
			}
		}
		if ok {
			return nil
		}
		time.Sleep(delay)
		if delay < time.Second {
			delay *= 2
		}
	}
	return fmt.Errorf("health check (%s %s) did not pass within %s; is the app listening on 0.0.0.0:%d ($PORT)?", kind, addr, timeout, port)
}

func (r *Runtime) isRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.containers[id]
	return m != nil && m.running
}

// Stop stops the task but keeps the container (used for sleep and eviction).
func (r *Runtime) Stop(ctx context.Context, id string, grace time.Duration) error {
	r.mu.Lock()
	if m := r.containers[id]; m != nil {
		m.Stopped = true
		if m.cancel != nil {
			m.cancel()
		}
	}
	r.mu.Unlock()
	ctx = r.ctx(ctx)
	ctr, err := r.client.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		return nil
	}
	if grace <= 0 {
		grace = 10 * time.Second
	}
	exitCh, err := task.Wait(r.ctx(context.Background()))
	if err == nil {
		_ = task.Kill(ctx, syscall.SIGTERM)
		select {
		case <-exitCh:
		case <-time.After(grace):
			_ = task.Kill(ctx, syscall.SIGKILL, client.WithKillAll)
			<-exitCh
		}
	}
	_, err = task.Delete(ctx, client.WithProcessKill)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	r.mu.Lock()
	if m := r.containers[id]; m != nil {
		m.running = false
		r.saveState(id, m)
	}
	r.mu.Unlock()
	return nil
}

// Remove stops and deletes a container, its network attachment and files.
func (r *Runtime) Remove(ctx context.Context, id string, grace time.Duration) error {
	if err := r.Stop(ctx, id, grace); err != nil {
		slog.Warn("stop before remove", "id", id, "err", err)
	}
	r.mu.Lock()
	m := r.containers[id]
	delete(r.containers, id)
	r.mu.Unlock()
	cctx := r.ctx(ctx)
	if ctr, err := r.client.LoadContainer(cctx, id); err == nil {
		if err := ctr.Delete(cctx, client.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
			slog.Warn("delete container", "id", id, "err", err)
		}
	}
	if m != nil && m.NetNS != "" {
		if err := r.net.Detach(ctx, m.Req.NetIndex, id, m.NetNS); err != nil {
			slog.Warn("cni detach", "id", id, "err", err)
		}
		_ = netns.LoadNetNS(m.NetNS).Remove()
	}
	return os.RemoveAll(r.containerDir(id))
}

// Recover reloads containers managed before an agent restart and restarts stopped ones.
func (r *Runtime) Recover(ctx context.Context) {
	cctx := r.ctx(ctx)
	ctrs, err := r.client.Containers(cctx, fmt.Sprintf("labels.%q==true", labelManaged))
	if err != nil {
		slog.Error("list containers", "err", err)
		return
	}
	for _, ctr := range ctrs {
		id := ctr.ID()
		data, err := os.ReadFile(filepath.Join(r.containerDir(id), "state.json"))
		if err != nil {
			slog.Warn("orphan container without state, removing", "id", id)
			_ = ctr.Delete(cctx, client.WithSnapshotCleanup)
			continue
		}
		m := &managed{}
		if err := json.Unmarshal(data, m); err != nil {
			continue
		}
		r.mu.Lock()
		r.containers[id] = m
		r.mu.Unlock()
		r.net.Restore(m.Req.NetIndex)
		running := false
		if task, err := ctr.Task(cctx, nil); err == nil {
			if st, err := task.Status(cctx); err == nil && st.Status == client.Running {
				running = true
				exitCh, err := task.Wait(r.ctx(context.Background()))
				if err == nil {
					wctx, cancel := context.WithCancel(context.Background())
					r.mu.Lock()
					m.running = true
					m.cancel = cancel
					r.mu.Unlock()
					go r.watch(wctx, id, exitCh)
				}
			}
		}
		if !running && !m.Stopped && !m.Evicted {
			slog.Info("restarting recovered container", "id", id)
			if err := r.startTask(cctx, id); err != nil {
				slog.Error("restart recovered container", "id", id, "err", err)
			}
		}
	}
	if err := r.net.SyncFirewall(); err != nil {
		slog.Warn("firewall", "err", err)
	}
}

// Statuses reports every managed container with fresh resource usage.
func (r *Runtime) Statuses(ctx context.Context) []proto.ContainerStatus {
	cctx := r.ctx(ctx)
	r.mu.Lock()
	ids := make([]string, 0, len(r.containers))
	for id := range r.containers {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	out := make([]proto.ContainerStatus, 0, len(ids))
	for _, id := range ids {
		r.collectMetrics(cctx, id)
		r.mu.Lock()
		m := r.containers[id]
		if m == nil || m.Req.Once {
			// Jobs are tracked by the request that started them, not as instances.
			r.mu.Unlock()
			continue
		}
		out = append(out, proto.ContainerStatus{
			ContainerID: id, InstanceID: m.Req.InstanceID, ServiceID: m.Req.ServiceID,
			Running: m.running || m.Stopped, ExitCode: m.ExitCode, Restarts: m.Restarts, IP: m.IP,
			MemoryMB: m.memMB, CPUMillis: m.cpuMillis, Evicted: m.Evicted,
		})
		r.mu.Unlock()
	}
	return out
}

// Evict stops a container to relieve memory pressure; the control plane is told via heartbeat.
func (r *Runtime) Evict(ctx context.Context, id string) error {
	r.mu.Lock()
	if m := r.containers[id]; m != nil {
		m.Evicted = true
	}
	r.mu.Unlock()
	return r.Stop(ctx, id, 5*time.Second)
}

// Snapshot returns QoS class and memory usage for eviction decisions.
func (r *Runtime) Snapshot() map[string]struct {
	QoS   string
	MemMB int
	Live  bool
} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]struct {
		QoS   string
		MemMB int
		Live  bool
	}{}
	for id, m := range r.containers {
		out[id] = struct {
			QoS   string
			MemMB int
			Live  bool
		}{m.Req.QoS, m.memMB, m.running && !m.Stopped && !m.Evicted}
	}
	return out
}

// GCImages removes images with the given prefix that are not in keep and not used by containers.
func (r *Runtime) GCImages(ctx context.Context, prefix string, keep []string) (int, error) {
	cctx := r.ctx(ctx)
	keepSet := map[string]bool{}
	for _, k := range keep {
		keepSet[k] = true
	}
	r.mu.Lock()
	for _, m := range r.containers {
		keepSet[m.Req.Image] = true
	}
	r.mu.Unlock()
	imgs, err := r.client.ImageService().List(cctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, img := range imgs {
		if !strings.HasPrefix(img.Name, prefix) || keepSet[img.Name] {
			continue
		}
		if err := r.client.ImageService().Delete(cctx, img.Name, images.SynchronousDelete()); err == nil {
			removed++
		}
	}
	return removed, nil
}

func rotateLog(path string) {
	st, err := os.Stat(path)
	if err != nil || st.Size() < 10<<20 {
		return
	}
	_ = os.Rename(path, path+".1")
}

// RunJob runs a container to completion and returns its exit code and output tail.
func (r *Runtime) RunJob(ctx context.Context, req proto.RunRequest) proto.JobReply {
	req.Once = true
	if _, err := r.Run(ctx, req); err != nil {
		return proto.JobReply{ContainerID: req.ContainerID, ExitCode: -1, Error: err.Error()}
	}
	r.mu.Lock()
	var done <-chan struct{}
	if m := r.containers[req.ContainerID]; m != nil && m.exited != nil {
		done = m.exited
	}
	r.mu.Unlock()
	timeout := time.Duration(req.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	rep := proto.JobReply{ContainerID: req.ContainerID}
	if done != nil {
		select {
		case <-done:
		case <-time.After(timeout):
			rep.Error = "job timed out after " + timeout.String()
		case <-ctx.Done():
			rep.Error = "cancelled"
		}
	}
	r.mu.Lock()
	if m := r.containers[req.ContainerID]; m != nil {
		rep.ExitCode = m.ExitCode
	}
	r.mu.Unlock()
	if rep.Error != "" {
		rep.ExitCode = -1
	}
	lines, _ := r.Logs(req.ContainerID, 200)
	rep.Output = strings.Join(lines, "\n")
	_ = r.Remove(context.Background(), req.ContainerID, 5*time.Second)
	return rep
}
