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
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
)

const (
	labelInstance = "paas.instance"
	labelService  = "paas.service"
	labelManaged  = "paas.managed"
	labelHAPG     = "paas.ha-postgres"
)

// imageAPI is the part of the containerd client that image handling uses (faked in tests).
type imageAPI interface {
	GetImage(ctx context.Context, ref string) (client.Image, error)
	Pull(ctx context.Context, ref string, opts ...client.RemoteOpt) (client.Image, error)
}

// Runtime manages containers on this node.
type Runtime struct {
	client    *client.Client
	images    imageAPI
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
	Req         proto.RunRequest `json:"req"`
	ImageDigest string           `json:"image_digest,omitempty"` // platform manifest digest of the image it was created from
	IP          string           `json:"ip"`
	NetNS       string           `json:"netns"`
	Restarts    int              `json:"restarts"`
	Stopped     bool             `json:"stopped"`
	Evicted     bool             `json:"evicted"`
	Fenced      bool             `json:"fenced,omitempty"` // stopped by the agent's lease fence; only an unfence ack starts it again
	ExitCode    int              `json:"exit_code"`
	lastCPU     uint64
	lastCPUAt   time.Time
	cpuMillis   int
	memMB       int
	running     bool
	cancel      context.CancelFunc
	exited      chan struct{} // closed when a run-once container exits
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
		images:     c,
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
// An image that is already here is never pulled again: use RefreshImage to move a tag.
func (r *Runtime) EnsureImage(ctx context.Context, ref string, pull bool, plainHost string) (client.Image, error) {
	ctx = r.ctx(ctx)
	img, err := r.images.GetImage(ctx, ref)
	if err == nil {
		return img, nil
	}
	if !errdefs.IsNotFound(err) || !pull {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	return r.pull(ctx, ref, plainHost)
}

func (r *Runtime) pull(ctx context.Context, ref, plainHost string) (client.Image, error) {
	slog.Info("pulling image", "ref", ref)
	return r.images.Pull(ctx, ref, client.WithPullUnpack, client.WithResolver(r.resolver(ctx, plainHost)))
}

// RefreshImage pulls ref even when it is already on the node, so a moving tag (postgres:17) picks
// up the newest minor, and returns the digest of the platform manifest it now points at. A failed
// pull leaves the tag as it was.
func (r *Runtime) RefreshImage(ctx context.Context, ref, plainHost string) (string, error) {
	ctx = r.ctx(ctx)
	img, err := r.pull(ctx, ref, plainHost)
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", ref, err)
	}
	return platformDigest(ctx, img)
}

// platformDigest is the digest of the manifest for this machine's platform: the same for two
// nodes of one architecture, unlike the digest of a multi-arch index.
func platformDigest(ctx context.Context, img client.Image) (string, error) {
	target := img.Target()
	if images.IsManifestType(target.MediaType) {
		return target.Digest.String(), nil
	}
	if !images.IsIndexType(target.MediaType) {
		return "", fmt.Errorf("image %s: unexpected media type %s", img.Name(), target.MediaType)
	}
	raw, err := content.ReadBlob(ctx, img.ContentStore(), target)
	if err != nil {
		return "", err
	}
	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return "", err
	}
	plat := img.Platform()
	for _, d := range idx.Manifests {
		if d.Platform != nil && images.IsManifestType(d.MediaType) && plat.Match(*d.Platform) {
			return d.Digest.String(), nil
		}
	}
	return "", fmt.Errorf("image %s has no manifest for this platform", img.Name())
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
	if m.ImageDigest, err = platformDigest(r.ctx(ctx), img); err != nil {
		slog.Warn("image digest unknown", "image", req.Image, "err", err) // reported empty: the service stays on its tag
	}
	nsh, ip, err := r.attachNet(ctx, id, m)
	if err != nil {
		return "", err
	}
	m.NetNS, m.IP = nsh.GetPath(), ip
	ok := false
	defer func() {
		if !ok {
			// Not ctx: a cancelled request must still release the address.
			_ = r.net.Detach(context.WithoutCancel(ctx), req.NetIndex, id, m.NetNS)
			_ = nsh.Remove()
			_ = os.RemoveAll(r.containerDir(id))
		}
	}()
	if err := r.writeNetFiles(id, req); err != nil {
		return "", err
	}

	cctx := r.ctx(ctx)
	if err := r.createContainer(cctx, img, m); err != nil {
		return "", err
	}
	ok = true
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

// attachNet creates a fresh netns for the container and attaches it to its project network.
func (r *Runtime) attachNet(ctx context.Context, id string, m *managed) (*netns.NetNS, string, error) {
	nsh, err := netns.NewNetNS(filepath.Join(r.dataDir, "netns"))
	if err != nil {
		return nil, "", fmt.Errorf("create netns: %w", err)
	}
	// Release any address left behind by an earlier attempt with the same container id.
	_ = r.net.Detach(ctx, m.Req.NetIndex, id, nsh.GetPath())
	ip, err := r.net.Attach(ctx, m.Req.NetIndex, id, nsh.GetPath())
	if err != nil {
		_ = nsh.Remove()
		return nil, "", err
	}
	return nsh, ip, nil
}

// ensureNetNS re-attaches the container when its netns is gone. A reboot
// unmounts every netns; the path baked into the spec must follow the new one.
func (r *Runtime) ensureNetNS(ctx context.Context, ctr client.Container, id string) error {
	r.mu.Lock()
	m := r.containers[id]
	r.mu.Unlock()
	if m == nil {
		return nil
	}
	closed, err := netns.LoadNetNS(m.NetNS).Closed()
	if err != nil {
		return err
	}
	if !closed {
		return nil
	}
	slog.Info("netns gone, re-attaching", "id", id)
	nsh, ip, err := r.attachNet(ctx, id, m)
	if err != nil {
		return err
	}
	undo := func() {
		_ = r.net.Detach(context.WithoutCancel(ctx), m.Req.NetIndex, id, nsh.GetPath())
		_ = nsh.Remove()
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		undo()
		return err
	}
	if spec.Linux != nil {
		for i := range spec.Linux.Namespaces {
			if spec.Linux.Namespaces[i].Type == specs.NetworkNamespace {
				spec.Linux.Namespaces[i].Path = nsh.GetPath()
			}
		}
	}
	if err := ctr.Update(ctx, client.UpdateContainerOpts(client.WithSpec(spec))); err != nil {
		undo()
		return fmt.Errorf("update spec: %w", err)
	}
	r.mu.Lock()
	m.NetNS, m.IP = nsh.GetPath(), ip
	r.saveState(id, m)
	r.mu.Unlock()
	return nil
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

// runtimeFor fails closed: a sandboxed service never silently falls back to
// runc, because standard is not a boundary for untrusted code.
func (r *Runtime) runtimeFor(class string) (string, any, error) {
	if class == model.RuntimeSandboxed {
		if _, err := exec.LookPath("containerd-shim-runsc-v1"); err != nil {
			return "", nil, fmt.Errorf("sandboxed runtime requested but gVisor (containerd-shim-runsc-v1) is not installed on this node")
		}
		return "io.containerd.runsc.v1", nil, nil
	}
	return "io.containerd.runc.v2", &options.Options{BinaryName: r.ociBinary}, nil
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
	var volumeDirs []string
	for _, v := range req.Volumes {
		host := filepath.Join(r.dataDir, "volumes", req.ServiceID, v.Name)
		if err := os.MkdirAll(host, 0o755); err != nil {
			return err
		}
		volumeDirs = append(volumeDirs, host)
		mounts = append(mounts, specs.Mount{Destination: v.Path, Type: "bind", Source: host, Options: []string{"rbind", "rw", "nosuid", "nodev"}})
	}

	specOpts := []oci.SpecOpts{
		oci.WithDefaultSpec(),
		oci.WithDefaultUnixDevices,
		oci.WithImageConfig(img),
		withVolumeOwner(volumeDirs),
		oci.WithEnv(env),
		oci.WithHostname(firstNonEmpty(req.Hostname, req.ContainerID[:12])),
		withoutRunTmpfs,
		withNoFileLimit,
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
	rtName, rtOpts, err := r.runtimeFor(req.RuntimeClass)
	if err != nil {
		return err
	}
	labels := map[string]string{labelManaged: "true", labelInstance: req.InstanceID, labelService: req.ServiceID}
	if req.HAPostgres {
		labels[labelHAPG] = "true"
	}
	_, err = r.client.NewContainer(ctx, req.ContainerID,
		client.WithImage(img),
		client.WithNewSnapshot(req.ContainerID+"-snap", img),
		client.WithRuntime(rtName, rtOpts),
		client.WithContainerLabels(labels),
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
	if err := r.ensureNetNS(ctx, ctr, id); err != nil {
		return fmt.Errorf("netns: %w", err)
	}
	if t, err := ctr.Task(ctx, nil); err == nil {
		_, _ = t.Delete(ctx, client.WithProcessKill)
	}
	logPath := r.LogPath(id)
	capLog(logPath)
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
			// An orphan left by a failed Remove already released its address. One left by an
			// agent crash inside Run did not, and its NetIndex is unknown (no state.json),
			// so release by container id.
			if n := r.net.ReleaseLeases(id); n > 0 {
				slog.Info("released orphan ip leases", "id", id, "n", n)
			}
			if t, err := ctr.Task(cctx, nil); err == nil {
				_, _ = t.Delete(cctx, client.WithProcessKill)
			}
			if spec, err := ctr.Spec(cctx); err == nil && spec.Linux != nil {
				for _, n := range spec.Linux.Namespaces {
					if n.Type == specs.NetworkNamespace && n.Path != "" {
						_ = netns.LoadNetNS(n.Path).Remove()
					}
				}
			}
			if err := ctr.Delete(cctx, client.WithSnapshotCleanup); err != nil {
				slog.Warn("delete orphan container", "id", id, "err", err)
			}
			continue
		}
		m := &managed{}
		if err := json.Unmarshal(data, m); err != nil {
			continue
		}
		r.mu.Lock()
		r.containers[id] = m
		r.mu.Unlock()
		if m.Req.Once {
			slog.Info("dropping job left from before restart", "id", id)
			_ = r.Remove(ctx, id, time.Second)
			continue
		}
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
			MemoryMB: m.memMB, CPUMillis: m.cpuMillis, Evicted: m.Evicted, ImageDigest: m.ImageDigest,
		})
		r.mu.Unlock()
	}
	return out
}

// HAPostgresRunning lists the HA postgres containers that are running and not fenced.
func (r *Runtime) HAPostgresRunning() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id, m := range r.containers {
		if m.Req.HAPostgres && m.running && !m.Fenced {
			ids = append(ids, id)
		}
	}
	return ids
}

// Fence stops a container and remembers it (persisted: a restarted agent must not start it).
func (r *Runtime) Fence(ctx context.Context, id string, grace time.Duration) error {
	r.mu.Lock()
	m := r.containers[id]
	if m == nil {
		r.mu.Unlock()
		return nil
	}
	m.Fenced = true
	r.saveState(id, m)
	r.mu.Unlock()
	err := r.Stop(ctx, id, grace)
	r.mu.Lock()
	r.saveState(id, m) // now with Stopped=true
	r.mu.Unlock()
	r.changed()
	return err
}

// Unfence starts a fenced container again (no-op when it is not fenced).
func (r *Runtime) Unfence(ctx context.Context, id string) error {
	r.mu.Lock()
	m := r.containers[id]
	if m == nil || !m.Fenced {
		r.mu.Unlock()
		return nil
	}
	m.Fenced = false
	r.saveState(id, m)
	r.mu.Unlock()
	if err := r.startTask(r.ctx(ctx), id); err != nil {
		r.mu.Lock()
		m.Fenced = true // retried by the next ack
		r.saveState(id, m)
		r.mu.Unlock()
		return err
	}
	r.changed()
	return nil
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

// withVolumeOwner hands empty, root-owned volume directories to the user the
// image runs as (resolved by WithImageConfig), so images with a non-root USER
// such as n8n (node, uid 1000) can write to their data directory.
func withVolumeOwner(dirs []string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Process == nil || s.Process.User.UID == 0 {
			return nil
		}
		uid, gid := int(s.Process.User.UID), int(s.Process.User.GID)
		for _, d := range dirs {
			st, err := os.Stat(d)
			if err != nil {
				continue
			}
			sys, ok := st.Sys().(*syscall.Stat_t)
			if !ok || sys.Uid != 0 {
				continue // already owned by someone: leave it alone
			}
			if entries, err := os.ReadDir(d); err != nil || len(entries) > 0 {
				continue
			}
			if err := os.Chown(d, uid, gid); err != nil {
				return fmt.Errorf("chown volume %s: %w", d, err)
			}
		}
		return nil
	}
}

// withoutRunTmpfs drops the empty tmpfs containerd's default spec mounts on
// /run: like Docker, keep the image's /run (MariaDB ships /run/mysqld there).
func withoutRunTmpfs(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
	kept := s.Mounts[:0]
	for _, m := range s.Mounts {
		if m.Destination == "/run" && m.Type == "tmpfs" {
			continue
		}
		kept = append(kept, m)
	}
	s.Mounts = kept
	return nil
}

// withNoFileLimit raises the open-files limit to Docker's default (containerd's
// spec keeps 1024, and apps such as n8n fail with EMFILE).
func withNoFileLimit(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
	if s.Process == nil {
		return nil
	}
	n := uint64(1048576)
	// A container cannot exceed the limit of the runtime that starts it.
	var cur syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &cur); err == nil && cur.Max < n {
		n = cur.Max
	}
	kept := s.Process.Rlimits[:0]
	for _, r := range s.Process.Rlimits {
		if r.Type != "RLIMIT_NOFILE" {
			kept = append(kept, r)
		}
	}
	s.Process.Rlimits = append(kept, specs.POSIXRlimit{Type: "RLIMIT_NOFILE", Hard: n, Soft: n})
	return nil
}

// Pause freezes a container's processes (cgroup freezer), for a consistent
// copy of its volumes; Resume thaws it. Both are no-ops for unknown containers.
func (r *Runtime) Pause(ctx context.Context, id string) error {
	ctx = r.ctx(ctx)
	ctr, err := r.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		return err
	}
	return task.Pause(ctx)
}

func (r *Runtime) Resume(ctx context.Context, id string) error {
	ctx = r.ctx(ctx)
	ctr, err := r.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		return err
	}
	return task.Resume(ctx)
}
