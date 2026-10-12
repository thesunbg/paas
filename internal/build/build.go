// Package build turns a git commit into a runnable image (or a static site
// directory) using git and BuildKit.
package build

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thesunbg/paas/internal/detect"
	"github.com/thesunbg/paas/internal/proto"
)

// Builder runs builds on a build node.
type Builder struct {
	BuildkitAddr  string
	WorkDir       string // scratch space for checkouts
	StaticDir     string // where static site outputs are stored
	AllowLocalGit bool
	sem           chan struct{}
}

func New(buildkitAddr, dataDir string, concurrency int) *Builder {
	if concurrency <= 0 {
		concurrency = 2
	}
	return &Builder{
		BuildkitAddr: buildkitAddr,
		WorkDir:      filepath.Join(dataDir, "builds"),
		StaticDir:    filepath.Join(dataDir, "static"),
		sem:          make(chan struct{}, concurrency),
	}
}

// Logger receives build output lines.
type Logger func(line string)

// Build executes the full pipeline and returns the result.
func (b *Builder) Build(ctx context.Context, req proto.BuildRequest, log Logger) (rep proto.BuildReply) {
	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		return proto.BuildReply{Error: ctx.Err().Error()}
	default:
		log("waiting for a free build slot...")
		select {
		case b.sem <- struct{}{}:
		case <-ctx.Done():
			return proto.BuildReply{Error: ctx.Err().Error()}
		}
	}
	defer func() { <-b.sem }()

	start := time.Now()
	work := filepath.Join(b.WorkDir, req.DeploymentID)
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return proto.BuildReply{Error: err.Error()}
	}
	defer os.RemoveAll(work)

	src := filepath.Join(work, "src")
	sha, msg, err := b.checkout(ctx, req, src, log)
	if err != nil {
		return proto.BuildReply{Error: err.Error()}
	}
	rep.CommitSHA, rep.CommitMessage = sha, msg

	root := src
	if req.RootDir != "" && req.RootDir != "/" && req.RootDir != "." {
		root = filepath.Join(src, filepath.Clean("/"+req.RootDir))
		if !strings.HasPrefix(root, src) {
			return proto.BuildReply{Error: "invalid root directory"}
		}
		if _, err := os.Stat(root); err != nil {
			return proto.BuildReply{Error: fmt.Sprintf("root directory %q not found in repository", req.RootDir)}
		}
	}

	keys := make([]string, 0, len(req.BuildEnv))
	for k := range req.BuildEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	plan, err := detect.Detect(root, detect.Options{ServiceType: req.ServiceType, Build: req.Build, Dockerfile: req.Dockerfile, BuildEnvKeys: keys})
	if err != nil {
		return proto.BuildReply{Error: err.Error()}
	}
	rep.Detected = plan.Summary()
	rep.Port = plan.Port
	log(fmt.Sprintf("==> detected %s", plan.Summary()))

	dfDir := root
	dfName := plan.UserDockerfile
	if plan.Dockerfile != "" {
		dfDir = filepath.Join(work, "dockerfile")
		if err := os.MkdirAll(dfDir, 0o755); err != nil {
			return proto.BuildReply{Error: err.Error()}
		}
		dfName = "Dockerfile"
		if err := os.WriteFile(filepath.Join(dfDir, dfName), []byte(plan.Dockerfile), 0o644); err != nil {
			return proto.BuildReply{Error: err.Error()}
		}
		log("==> generated Dockerfile:")
		for _, l := range strings.Split(strings.TrimSpace(plan.Dockerfile), "\n") {
			log("    " + l)
		}
		ensureDockerignore(root)
	}

	envFile := filepath.Join(work, "build.env")
	if err := writeEnvFile(envFile, req.BuildEnv); err != nil {
		return proto.BuildReply{Error: err.Error()}
	}

	args := []string{"--addr", b.BuildkitAddr, "build", "--progress", "plain",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + root,
		"--local", "dockerfile=" + dfDir,
		"--opt", "filename=" + dfName,
		"--secret", "id=paasenv,src=" + envFile,
	}
	for _, k := range keys {
		args = append(args, "--opt", "build-arg:"+k+"="+req.BuildEnv[k])
	}
	if plan.Static {
		dest := filepath.Join(b.StaticDir, req.ServiceID, req.DeploymentID)
		_ = os.RemoveAll(dest)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return proto.BuildReply{Error: err.Error()}
		}
		args = append(args, "--output", "type=local,dest="+dest)
		rep.StaticPath = dest
	} else {
		args = append(args, "--output", "type=image,name="+req.ImageName+",unpack=true")
		rep.Image = req.ImageName
	}
	log("==> building with BuildKit")
	if err := runStreaming(ctx, log, "", nil, "buildctl", args...); err != nil {
		if rep.StaticPath != "" {
			_ = os.RemoveAll(rep.StaticPath)
		}
		return proto.BuildReply{Error: "build failed: " + err.Error(), CommitSHA: sha, CommitMessage: msg, Detected: rep.Detected}
	}
	if plan.Static {
		if _, err := os.Stat(filepath.Join(rep.StaticPath, "index.html")); err != nil {
			log("warning: build output has no index.html")
		}
	}
	log(fmt.Sprintf("==> build finished in %s", time.Since(start).Round(100*time.Millisecond)))
	return rep
}

func (b *Builder) checkout(ctx context.Context, req proto.BuildRequest, dest string, log Logger) (string, string, error) {
	repo := strings.TrimSpace(req.Repo)
	if repo == "" {
		return "", "", errors.New("repository url is empty")
	}
	if strings.HasPrefix(repo, "file://") || strings.HasPrefix(repo, "/") {
		if !b.AllowLocalGit {
			return "", "", errors.New("local repositories are disabled (set PAAS_ALLOW_LOCAL_GIT=true)")
		}
	} else if !strings.HasPrefix(repo, "https://") && !strings.HasPrefix(repo, "http://") {
		return "", "", errors.New("repository must be an https:// URL")
	}
	cloneURL := repo
	redact := func(s string) string { return s }
	if req.GitToken != "" && strings.HasPrefix(repo, "http") {
		u, err := url.Parse(repo)
		if err != nil {
			return "", "", err
		}
		u.User = url.UserPassword("x-access-token", req.GitToken)
		cloneURL = u.String()
		redact = func(s string) string { return strings.ReplaceAll(s, req.GitToken, "***") }
	}
	logR := func(l string) { log(redact(l)) }
	branch := req.Branch
	log(fmt.Sprintf("==> cloning %s (branch %s)", repo, firstNonEmpty(branch, "default")))
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, cloneURL, dest)
	if err := runStreaming(ctx, logR, "", env, "git", args...); err != nil {
		return "", "", fmt.Errorf("git clone failed: %s", redact(err.Error()))
	}
	if req.CommitSHA != "" {
		head, _ := gitOut(ctx, dest, "rev-parse", "HEAD")
		if !strings.HasPrefix(head, req.CommitSHA) {
			if err := runStreaming(ctx, logR, dest, env, "git", "fetch", "--depth", "1", "origin", req.CommitSHA); err != nil {
				return "", "", fmt.Errorf("fetch commit %s: %s", req.CommitSHA, redact(err.Error()))
			}
			if err := runStreaming(ctx, logR, dest, env, "git", "checkout", "--quiet", "FETCH_HEAD"); err != nil {
				return "", "", err
			}
		}
	}
	sha, _ := gitOut(ctx, dest, "rev-parse", "HEAD")
	msg, _ := gitOut(ctx, dest, "log", "-1", "--format=%s")
	if len(sha) >= 7 {
		log(fmt.Sprintf("==> commit %s %s", sha[:7], msg))
	}
	_ = os.RemoveAll(filepath.Join(dest, ".git"))
	return sha, msg, nil
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// runStreaming runs a command, forwarding combined output line by line.
func runStreaming(ctx context.Context, log Logger, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	var tail []string
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			log(line)
			mu.Lock()
			tail = append(tail, line)
			if len(tail) > 5 {
				tail = tail[1:]
			}
			mu.Unlock()
		}
	}()
	err := cmd.Run()
	pw.Close()
	<-done
	if err != nil {
		mu.Lock()
		defer mu.Unlock()
		return fmt.Errorf("%s: %v", name, err)
	}
	return nil
}

func writeEnvFile(path string, env map[string]string) error {
	var b strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s='%s'\n", k, strings.ReplaceAll(env[k], "'", `'"'"'`))
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func ensureDockerignore(root string) {
	p := filepath.Join(root, ".dockerignore")
	if _, err := os.Stat(p); err == nil {
		return
	}
	_ = os.WriteFile(p, []byte(".git\nnode_modules\n.next\n.nuxt\n__pycache__\n*.pyc\n.venv\nvendor/bundle\ntarget\n.env\n.env.*\n"), 0o644)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ImageName returns the local image reference for a deployment.
func ImageName(serviceID, deploymentID string) string {
	return fmt.Sprintf("paas.local/%s:%s", serviceID, deploymentID)
}

