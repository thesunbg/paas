package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/cron"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/scheduler"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// cronLoop starts scheduled jobs at the top of every minute (UTC).
func (s *Server) cronLoop(ctx context.Context) {
	for {
		now := time.Now().UTC()
		next := now.Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		services, err := s.store.AllServices(ctx)
		if err != nil {
			continue
		}
		for _, svc := range services {
			if svc.Type != model.TypeCron || svc.Status != model.StatusRunning || svc.CurrentDeploymentID == nil {
				continue
			}
			sched, err := cron.Parse(svc.Spec.Schedule)
			if err != nil || !sched.Matches(next) {
				continue
			}
			if busy, _ := s.store.RunningJob(ctx, svc.ID); busy {
				slog.Info("cron: previous run still in progress, skipping", "service", svc.Slug)
				continue
			}
			go func(svc store.Service) {
				if _, err := s.runJob(context.Background(), &svc, "schedule", true); err != nil {
					slog.Warn("cron job", "service", svc.Slug, "err", err)
				}
			}(svc)
		}
	}
}

// runJob starts one run of a cron service. With wait=false it returns as soon as the run is recorded.
func (s *Server) runJob(ctx context.Context, svc *store.Service, trigger string, wait bool) (*store.JobRun, error) {
	if svc.CurrentDeploymentID == nil {
		return nil, errors.New("job has not been deployed yet")
	}
	d, err := s.store.DeploymentByID(ctx, *svc.CurrentDeploymentID)
	if err != nil {
		return nil, err
	}
	proj, err := s.store.ProjectByID(ctx, svc.ProjectID)
	if err != nil {
		return nil, err
	}
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	committed, _ := s.store.NodeCommitted(ctx)
	node, err := scheduler.Pick(nodes, committed, scheduler.Request{Resources: svc.Spec.Resources, QoS: svc.Spec.QoS, RuntimeClass: svc.Spec.RuntimeClass})
	if err != nil {
		return nil, err
	}
	image, plain, err := s.imageFor(ctx, d, node, "")
	if err != nil {
		return nil, err
	}
	env, err := s.runtimeEnv(ctx, svc, proj)
	if err != nil {
		return nil, err
	}
	containerID := fmt.Sprintf("%s-job-%s", util.Slugify(svc.Slug), util.RandomToken(4))
	run, err := s.store.CreateJobRun(ctx, svc.ID, node.ID, containerID, trigger)
	if err != nil {
		return nil, err
	}
	timeout := svc.Spec.JobTimeoutSec
	if timeout <= 0 {
		timeout = 3600
	}
	req := proto.RunRequest{
		InstanceID: run.ID, ServiceID: svc.ID, ContainerID: containerID, NetIndex: proj.NetIndex, DNSSearch: proj.Slug + ".internal", Hostname: svc.Slug,
		Image: image, PullImage: !strings.HasPrefix(image, "paas.local/"), PlainHTTPRegistry: plain, Env: env,
		Command: svc.Spec.Command, Resources: svc.Spec.Resources, QoS: svc.Spec.QoS, RuntimeClass: svc.Spec.RuntimeClass,
		Once: true, TimeoutSec: timeout,
	}
	exec := func() {
		var rep proto.JobReply
		err := s.requestAgent(context.Background(), node.ID, proto.CmdJob, req, &rep, time.Duration(timeout)*time.Second+15*time.Minute)
		status := "succeeded"
		if err != nil {
			rep.Error, rep.ExitCode = err.Error(), -1
		}
		if rep.Error != "" || rep.ExitCode != 0 {
			status = "failed"
		}
		out := rep.Output
		if rep.Error != "" {
			out += "\n" + rep.Error
		}
		_ = s.store.FinishJobRun(context.Background(), run.ID, status, rep.ExitCode, out)
		if status == "failed" {
			s.notify(context.Background(), proj.TeamID, "job.failed", fmt.Sprintf("Job %s/%s failed (exit %d)", proj.Slug, svc.Slug, rep.ExitCode), map[string]any{"service": svc.Slug, "run": run.ID})
		}
	}
	if wait {
		exec()
		return s.store.JobRunByID(ctx, run.ID)
	}
	go exec()
	return run, nil
}
