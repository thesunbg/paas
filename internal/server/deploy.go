package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/build"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/resources"
	rt "github.com/thesunbg/paas/internal/runtime"
	"github.com/thesunbg/paas/internal/scheduler"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// deployKick wakes the deploy worker when a deployment is queued.
var deployKick = make(chan struct{}, 1)

func kickDeploys() {
	select {
	case deployKick <- struct{}{}:
	default:
	}
}

// QueueDeployment records a new deployment and wakes the worker.
func (s *Server) QueueDeployment(ctx context.Context, svc *store.Service, trigger, commitSHA, commitMsg string, userID *string) (*store.Deployment, error) {
	d, err := s.store.CreateDeployment(ctx, &store.Deployment{
		ServiceID: svc.ID, Status: model.DeployQueued, Trigger: trigger, CommitSHA: commitSHA,
		CommitMessage: commitMsg, Branch: svc.Source.Branch, CreatedBy: userID,
	})
	if err != nil {
		return nil, err
	}
	// A newer commit makes older queued deployments of the same service pointless.
	_ = s.store.CancelOlderQueued(ctx, svc.ID, d.ID)
	s.hub.Log(d.ID, fmt.Sprintf("==> deployment queued (%s)", trigger))
	kickDeploys()
	return d, nil
}

func (s *Server) deployWorker(ctx context.Context) {
	sem := make(chan struct{}, 8) // concurrent deployments across services
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		for {
			d, err := s.store.ClaimQueuedDeployment(ctx)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				slog.Error("claim deployment", "err", err)
				break
			}
			sem <- struct{}{}
			go func(d *store.Deployment) {
				defer func() { <-sem; kickDeploys() }()
				s.runDeployment(context.WithoutCancel(ctx), d)
			}(d)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-deployKick:
		}
	}
}

func (s *Server) failDeployment(ctx context.Context, d *store.Deployment, svc *store.Service, err error) {
	msg := err.Error()
	s.hub.Log(d.ID, "==> deployment failed: "+msg)
	_ = s.store.SetDeploymentStatus(ctx, d.ID, model.DeployFailed, msg)
	if svc != nil && svc.CurrentDeploymentID == nil {
		_ = s.store.SetServiceStatus(ctx, svc.ID, model.StatusFailed)
	} else if svc != nil {
		_ = s.store.SetServiceStatus(ctx, svc.ID, model.StatusRunning)
	}
	s.reportCommitStatus(ctx, svc, d, "failure")
}

func (s *Server) runDeployment(ctx context.Context, d *store.Deployment) {
	unlock := s.lock(d.ServiceID)
	defer unlock()
	svc, err := s.store.ServiceByID(ctx, d.ServiceID)
	if err != nil {
		_ = s.store.SetDeploymentStatus(ctx, d.ID, model.DeployFailed, "service not found")
		return
	}
	start := time.Now()
	if svc.Status != model.StatusRunning {
		_ = s.store.SetServiceStatus(ctx, svc.ID, model.StatusDeploying)
	}
	s.reportCommitStatus(ctx, svc, d, "pending")

	switch {
	case d.Image != "" || d.StaticPath != "":
		// Rollback / restart: reuse a previous artifact.
		s.hub.Log(d.ID, "==> reusing build artifact "+firstNonEmpty(d.Image, d.StaticPath))
		_ = s.store.UpdateDeploymentBuilt(ctx, d)
	case svc.Source.Kind == "git":
		if err := s.buildDeployment(ctx, svc, d); err != nil {
			s.failDeployment(ctx, d, svc, err)
			return
		}
	default:
		img := svc.Spec.Image
		if img == "" {
			img = svc.Source.Image
		}
		if img == "" {
			s.failDeployment(ctx, d, svc, errors.New("no image configured"))
			return
		}
		d.Image = rt.NormalizeRef(img)
		d.Detected = "image"
		s.hub.Log(d.ID, "==> using image "+d.Image)
		_ = s.store.UpdateDeploymentBuilt(ctx, d)
	}
	svc, _ = s.store.ServiceByID(ctx, d.ServiceID)

	if d.StaticPath != "" {
		if err := s.store.SetServiceCurrentDeployment(ctx, svc.ID, d.ID, model.StatusRunning); err != nil {
			s.failDeployment(ctx, d, svc, err)
			return
		}
		// A service that switched to static no longer needs containers.
		for _, in := range s.instancesOf(ctx, svc.ID) {
			s.stopInstance(ctx, &in, 5)
		}
		s.publishAll(ctx)
	} else {
		if err := s.rollout(ctx, svc, d); err != nil {
			s.failDeployment(ctx, d, svc, err)
			return
		}
	}
	_ = s.store.SetDeploymentStatus(ctx, d.ID, model.DeployActive, "")
	_ = s.store.SupersedeOthers(ctx, svc.ID, d.ID)
	s.hub.Log(d.ID, fmt.Sprintf("==> deployment live in %s", time.Since(start).Round(100*time.Millisecond)))
	if doms, err := s.store.DomainsForService(ctx, svc.ID); err == nil {
		for _, dom := range doms {
			s.hub.Log(d.ID, "    "+s.urlFor(dom.Hostname))
		}
	}
	s.reportCommitStatus(ctx, svc, d, "success")
	go s.cleanupStatic(context.Background(), svc.ID)
}

func (s *Server) urlFor(host string) string {
	if strings.HasPrefix(s.cfg.PublicURL, "https://") {
		return "https://" + host
	}
	return "http://" + host
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

func (s *Server) pickBuildNode(ctx context.Context) (*store.Node, error) {
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var best *store.Node
	for i := range nodes {
		n := &nodes[i]
		if n.Status != "online" || !n.HasRole("build") {
			continue
		}
		if best == nil || n.Usage.MemoryAvailableMB > best.Usage.MemoryAvailableMB {
			best = n
		}
	}
	if best == nil {
		return nil, errors.New("no online build node (role 'build') is available")
	}
	return best, nil
}

func (s *Server) buildDeployment(ctx context.Context, svc *store.Service, d *store.Deployment) error {
	node, err := s.pickBuildNode(ctx)
	if err != nil {
		return err
	}
	_ = s.store.UpdateDeploymentBuild(ctx, d.ID, node.ID)
	d.BuildNodeID = &node.ID
	token, err := s.box.Decrypt(svc.GitTokenEnc)
	if err != nil {
		return fmt.Errorf("decrypt git token: %w", err)
	}
	env, err := s.userEnv(ctx, svc)
	if err != nil {
		return err
	}
	s.hub.Log(d.ID, "==> building on node "+node.Name)
	req := proto.BuildRequest{
		DeploymentID: d.ID, ServiceID: svc.ID, ServiceType: svc.Type,
		Repo: svc.Source.Repo, Branch: svc.Source.Branch, CommitSHA: d.CommitSHA, GitToken: token,
		RootDir: svc.Source.RootDir, Dockerfile: svc.Source.Dockerfile, Build: svc.Build,
		ImageName: build.ImageName(svc.ID, d.ID), BuildEnv: env, LogSubject: proto.DeployLogSubject(d.ID),
	}
	var rep proto.BuildReply
	if err := s.requestAgent(ctx, node.ID, proto.CmdBuild, req, &rep, 50*time.Minute); err != nil {
		return err
	}
	if rep.CommitSHA != "" {
		d.CommitSHA, d.CommitMessage = rep.CommitSHA, rep.CommitMessage
	}
	if rep.Error != "" {
		d.Detected = rep.Detected
		_ = s.store.UpdateDeploymentBuilt(ctx, d)
		_ = s.store.SetDeploymentStatus(ctx, d.ID, model.DeployBuilding, "")
		return errors.New(rep.Error)
	}
	d.Image, d.StaticPath, d.Detected = rep.Image, rep.StaticPath, rep.Detected
	if err := s.store.UpdateDeploymentBuilt(ctx, d); err != nil {
		return err
	}
	// Remember the detected language (runtime tuning) and exposed port.
	lang := strings.SplitN(strings.Fields(rep.Detected + " ")[0], "/", 2)[0]
	changed := false
	if lang != "" && lang != "dockerfile" && svc.Spec.Language != lang {
		svc.Spec.Language = lang
		changed = true
	}
	if rep.Port > 0 && svc.Spec.Port == 8080 && rep.Port != 8080 && lang == "dockerfile" {
		svc.Spec.Port = rep.Port
		changed = true
		s.hub.Log(d.ID, fmt.Sprintf("==> using port %d from Dockerfile EXPOSE", rep.Port))
	}
	if changed {
		_ = s.store.SetServiceSpec(ctx, svc.ID, svc.Spec)
	}
	return nil
}

// userEnv decrypts the service's environment variables plus linked database URLs.
func (s *Server) userEnv(ctx context.Context, svc *store.Service) (map[string]string, error) {
	vars, err := s.store.EnvVars(ctx, svc.ID)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, v := range vars {
		plain, err := s.box.Decrypt(v.ValueEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", v.Key, err)
		}
		env[v.Key] = plain
	}
	links, err := s.store.LinksForService(ctx, svc.ID)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		db, err := s.store.DatabaseByID(ctx, l.DatabaseID)
		if err != nil || db.Status != "ready" {
			continue
		}
		if u, err := s.databaseURL(ctx, db); err == nil {
			if _, set := env[l.EnvName]; !set {
				env[l.EnvName] = u
			}
		}
	}
	return env, nil
}

// runtimeEnv is the full environment of a container: system, tuning, then user values.
func (s *Server) runtimeEnv(ctx context.Context, svc *store.Service, proj *store.Project) (map[string]string, error) {
	env := map[string]string{}
	for k, v := range resources.TuningEnv(svc.Spec.Language, svc.Spec.Resources) {
		env[k] = v
	}
	for k, v := range svc.Spec.Env {
		env[k] = v
	}
	if svc.Spec.Port > 0 {
		env["PORT"] = fmt.Sprint(svc.Spec.Port)
	}
	env["PAAS_SERVICE"] = svc.Slug
	env["PAAS_PROJECT"] = proj.Slug
	env["PAAS_INTERNAL_HOST"] = internalHost(svc.Slug, proj.Slug)
	user, err := s.userEnv(ctx, svc)
	if err != nil {
		return nil, err
	}
	for k, v := range user {
		env[k] = v
	}
	return env, nil
}

func (s *Server) instancesOf(ctx context.Context, serviceID string) []store.Instance {
	list, _ := s.store.InstancesForService(ctx, serviceID)
	return list
}

// startInstance schedules and starts one instance of a deployment, returning it when healthy.
func (s *Server) startInstance(ctx context.Context, svc *store.Service, proj *store.Project, d *store.Deployment, logID string, exclude map[string]bool) (*store.Instance, error) {
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	committed, err := s.store.NodeCommitted(ctx)
	if err != nil {
		return nil, err
	}
	spread := map[string]int{}
	for _, in := range s.instancesOf(ctx, svc.ID) {
		if in.DeploymentID != nil && *in.DeploymentID == d.ID {
			spread[in.NodeID]++
		}
	}
	pinned := ""
	if svc.NodeID != nil {
		pinned = *svc.NodeID
	}
	node, err := scheduler.Pick(nodes, committed, scheduler.Request{
		Resources: svc.Spec.Resources, QoS: svc.Spec.QoS, RuntimeClass: svc.Spec.RuntimeClass,
		PinnedNode: pinned, Spread: spread, Exclude: exclude,
	})
	if err != nil {
		return nil, err
	}
	env, err := s.runtimeEnv(ctx, svc, proj)
	if err != nil {
		return nil, err
	}
	containerID := fmt.Sprintf("%s-%s-%s", util.Slugify(svc.Slug), util.ShortID(d.ID), util.RandomToken(3))
	inst, err := s.store.CreateInstance(ctx, &store.Instance{
		ServiceID: svc.ID, DeploymentID: &d.ID, NodeID: node.ID, ContainerID: containerID, Port: svc.Spec.Port,
		Status: model.InstStarting, MemoryRequestMB: svc.Spec.Resources.MemoryRequestMB, CPURequestMillis: svc.Spec.Resources.CPURequestMillis,
	})
	if err != nil {
		return nil, err
	}
	if logID != "" {
		s.hub.Log(logID, fmt.Sprintf("==> starting instance %s on %s (%d MB, %s)", containerID, node.Name, svc.Spec.Resources.MemoryLimitMB, svc.Spec.QoS))
	}
	image, plainRegistry, err := s.imageFor(ctx, d, node, logID)
	if err != nil {
		_ = s.store.DeleteInstance(ctx, inst.ID)
		return nil, err
	}
	req := proto.RunRequest{
		InstanceID: inst.ID, ServiceID: svc.ID, ContainerID: containerID, NetIndex: proj.NetIndex,
		Hostname: svc.Slug, Image: image, PullImage: !strings.HasPrefix(image, "paas.local/"), PlainHTTPRegistry: plainRegistry,
		Env: env, Command: svc.Spec.Command, Port: svc.Spec.Port, Resources: svc.Spec.Resources,
		QoS: svc.Spec.QoS, RuntimeClass: svc.Spec.RuntimeClass, Volumes: svc.Spec.Volumes, HealthCheck: svc.Spec.HealthCheck,
	}
	if svc.Type == model.TypeWorker {
		req.Port = 0
	}
	var rep proto.RunReply
	err = s.requestAgent(ctx, node.ID, proto.CmdRun, req, &rep, 11*time.Minute)
	if err == nil && rep.Error != "" {
		err = errors.New(rep.Error)
	}
	if err != nil {
		_ = s.store.DeleteInstance(ctx, inst.ID)
		return nil, fmt.Errorf("instance on %s: %w", node.Name, err)
	}
	if err := s.store.SetInstanceRunning(ctx, inst.ID, rep.IP); err != nil {
		return nil, err
	}
	if len(svc.Spec.Volumes) > 0 && svc.NodeID == nil {
		// Stateful services stay on the node that now holds their volume.
		_ = s.store.SetServiceNode(ctx, svc.ID, &node.ID)
		svc.NodeID = &node.ID
	}
	inst.IP, inst.Status = rep.IP, model.InstRunning
	if logID != "" {
		s.hub.Log(logID, fmt.Sprintf("    healthy at %s", rep.IP))
	}
	return inst, nil
}

// stopInstance removes a container and its instance record.
func (s *Server) stopInstance(ctx context.Context, in *store.Instance, graceSec int) {
	_ = s.store.SetInstanceStatus(ctx, in.ID, model.InstStopping)
	var rep proto.GenericReply
	err := s.requestAgent(ctx, in.NodeID, proto.CmdStop, proto.StopRequest{ContainerID: in.ContainerID, GraceSec: graceSec}, &rep, time.Duration(graceSec+30)*time.Second)
	if err != nil {
		slog.Warn("stop instance", "container", in.ContainerID, "err", err)
		if node, nerr := s.store.NodeByID(ctx, in.NodeID); nerr == nil && node.Status == "online" {
			// Keep the record so a later reconcile can retry.
			_ = s.store.SetInstanceStatus(ctx, in.ID, model.InstLost)
			return
		}
	}
	_ = s.store.DeleteInstance(ctx, in.ID)
}

// rollout starts the new deployment, switches traffic and retires old instances.
func (s *Server) rollout(ctx context.Context, svc *store.Service, d *store.Deployment) error {
	proj, err := s.store.ProjectByID(ctx, svc.ProjectID)
	if err != nil {
		return err
	}
	old := s.instancesOf(ctx, svc.ID)
	replicas := svc.Spec.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	stateful := len(svc.Spec.Volumes) > 0
	if stateful && len(old) > 0 {
		// A volume cannot be shared by two processes: stop first, then start (recreate strategy).
		s.hub.Log(d.ID, "==> stateful service: stopping previous instance before starting the new one")
		for i := range old {
			s.stopInstance(ctx, &old[i], 30)
		}
		old = nil
		s.publishAll(ctx)
	}
	var started []*store.Instance
	for i := 0; i < replicas; i++ {
		in, err := s.startInstance(ctx, svc, proj, d, d.ID, nil)
		if err != nil {
			for _, st := range started {
				s.stopInstance(ctx, st, 2)
			}
			return err
		}
		started = append(started, in)
	}
	if err := s.store.SetServiceCurrentDeployment(ctx, svc.ID, d.ID, model.StatusRunning); err != nil {
		return err
	}
	s.publishAll(ctx)
	if len(old) > 0 {
		s.hub.Log(d.ID, fmt.Sprintf("==> traffic switched, draining %d old instance(s)", len(old)))
		time.Sleep(2 * time.Second) // let proxies pick up the new routes
		for i := range old {
			s.stopInstance(ctx, &old[i], 15)
		}
		s.publishDNS(ctx)
	}
	return nil
}

// restartService rolls the current deployment again (e.g. after env changes).
func (s *Server) restartService(ctx context.Context, svc *store.Service, userID *string) (*store.Deployment, error) {
	if svc.CurrentDeploymentID == nil {
		return nil, errors.New("service has no active deployment")
	}
	cur, err := s.store.DeploymentByID(ctx, *svc.CurrentDeploymentID)
	if err != nil {
		return nil, err
	}
	return s.redeploy(ctx, svc, cur, "restart", userID)
}

// redeploy queues a new deployment reusing the artifact of an existing one.
func (s *Server) redeploy(ctx context.Context, svc *store.Service, from *store.Deployment, trigger string, userID *string) (*store.Deployment, error) {
	if from.Image == "" && from.StaticPath == "" {
		return nil, errors.New("that deployment has no build artifact")
	}
	d, err := s.store.CreateDeployment(ctx, &store.Deployment{
		ServiceID: svc.ID, Status: model.DeployQueued, Trigger: trigger, CommitSHA: from.CommitSHA,
		CommitMessage: from.CommitMessage, Branch: from.Branch, Image: from.Image, StaticPath: from.StaticPath, CreatedBy: userID,
	})
	if err != nil {
		return nil, err
	}
	// Keep where the artifact lives so other nodes can still fetch it from the internal registry.
	_, _ = s.store.DB.Exec(ctx, `UPDATE deployments SET detected=$2, build_node_id=$3, pushed_image=$4 WHERE id=$1`,
		d.ID, from.Detected, from.BuildNodeID, from.PushedImage)
	s.hub.Log(d.ID, fmt.Sprintf("==> %s of deployment %s", trigger, util.ShortID(from.ID)))
	kickDeploys()
	return d, nil
}

// scaleService changes the replica count and converges immediately.
func (s *Server) scaleService(ctx context.Context, svc *store.Service, replicas int) error {
	if len(svc.Spec.Volumes) > 0 && replicas > 1 {
		return errors.New("services with volumes cannot run more than one replica")
	}
	svc.Spec.Replicas = replicas
	if err := s.store.SetServiceSpec(ctx, svc.ID, svc.Spec); err != nil {
		return err
	}
	go s.reconcileService(context.Background(), svc.ID)
	return nil
}

// stopService stops all instances and keeps the configuration.
func (s *Server) stopService(ctx context.Context, svc *store.Service) {
	unlock := s.lock(svc.ID)
	defer unlock()
	for _, in := range s.instancesOf(ctx, svc.ID) {
		s.stopInstance(ctx, &in, 10)
	}
	_ = s.store.SetServiceStatus(ctx, svc.ID, model.StatusStopped)
	s.publishAll(ctx)
}

// startService brings a stopped or sleeping service back.
func (s *Server) startService(ctx context.Context, svc *store.Service) error {
	if svc.CurrentDeploymentID == nil {
		return errors.New("service has never been deployed")
	}
	if err := s.store.SetServiceStatus(ctx, svc.ID, model.StatusRunning); err != nil {
		return err
	}
	s.reconcileService(ctx, svc.ID)
	return nil
}

// deleteService stops everything and removes the service.
func (s *Server) deleteService(ctx context.Context, svc *store.Service) error {
	s.stopService(ctx, svc)
	deps, _ := s.store.DeploymentsForService(ctx, svc.ID, 1000)
	var paths []string
	for _, d := range deps {
		if d.StaticPath != "" {
			paths = append(paths, d.StaticPath)
		}
	}
	if len(paths) > 0 {
		s.broadcastRemoveFiles(ctx, paths)
	}
	if err := s.store.DeleteService(ctx, svc.ID); err != nil {
		return err
	}
	s.publishAll(ctx)
	return nil
}

func (s *Server) broadcastRemoveFiles(ctx context.Context, paths []string) {
	nodes, _ := s.store.Nodes(ctx)
	for _, n := range nodes {
		if n.Status == "online" {
			_ = s.requestAgent(ctx, n.ID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: paths}, &proto.GenericReply{}, 30*time.Second)
		}
	}
}

// cleanupStatic removes static outputs older than the retention window.
func (s *Server) cleanupStatic(ctx context.Context, serviceID string) {
	deps, err := s.store.DeploymentsForService(ctx, serviceID, 200)
	if err != nil {
		return
	}
	kept := 0
	keep := map[string]bool{}
	var candidates []store.Deployment
	for _, d := range deps {
		if d.StaticPath == "" {
			continue
		}
		inFlight := d.Status == model.DeployQueued || d.Status == model.DeployBuilding || d.Status == model.DeployDeploying
		if d.Status == model.DeployActive || inFlight || kept < s.cfg.ImageRetention {
			kept++
			keep[d.StaticPath] = true
			continue
		}
		candidates = append(candidates, d)
	}
	var remove []string
	for _, d := range candidates {
		if !keep[d.StaticPath] {
			remove = append(remove, d.StaticPath)
			keep[d.StaticPath] = true // dedupe
		}
		_, _ = s.store.DB.Exec(ctx, `UPDATE deployments SET static_path='' WHERE id=$1`, d.ID)
	}
	if len(remove) > 0 {
		s.broadcastRemoveFiles(ctx, remove)
	}
}
