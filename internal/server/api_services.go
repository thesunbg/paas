package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/cron"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/resources"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

type serviceInput struct {
	Name          *string              `json:"name"`
	Type          string               `json:"type"`
	Source        *model.ServiceSource `json:"source"`
	Build         *model.BuildConfig   `json:"build"`
	Size          string               `json:"size"`
	QoS           string               `json:"qos"`
	Port          *int                 `json:"port"`
	Replicas      *int                 `json:"replicas"`
	Resources     *model.Resources     `json:"resources"`
	Sleep         *model.SleepPolicy   `json:"sleep"`
	HealthCheck   *model.HealthCheck   `json:"health_check"`
	RuntimeClass  *string              `json:"runtime_class"`
	Volumes       *[]model.Volume      `json:"volumes"`
	Command       *[]string            `json:"command"`
	SPA           *bool                `json:"spa"`
	Env           map[string]string    `json:"env"`
	GitToken      *string              `json:"git_token"`
	Domain        string               `json:"domain"`
	Deploy        *bool                `json:"deploy"`
	AutoDeploy    *bool                `json:"auto_deploy"`
	Previews      *bool                `json:"previews"`
	Autoscale     *model.Autoscale     `json:"autoscale"`
	Schedule      *string              `json:"schedule"`
	JobTimeoutSec *int                 `json:"job_timeout_sec"`
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// applyInput merges API input into a service, validating everything.
func (s *Server) applyInput(svc *store.Service, in *serviceInput) error {
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			return errors.New("name required")
		}
		svc.Name = strings.TrimSpace(*in.Name)
	}
	if in.Source != nil {
		src := *in.Source
		src.AutoDeploy = svc.Source.AutoDeploy
		if src.Kind == "" {
			src.Kind = "git"
			if src.Image != "" && src.Repo == "" {
				src.Kind = "image"
			}
		}
		switch src.Kind {
		case "git":
			if src.Repo == "" {
				return errors.New("source.repo required")
			}
			if src.Branch == "" {
				src.Branch = "main"
			}
		case "image":
			if src.Image == "" {
				return errors.New("source.image required")
			}
		default:
			return errors.New("source.kind must be git or image")
		}
		svc.Source = src
	}
	if in.AutoDeploy != nil {
		svc.Source.AutoDeploy = *in.AutoDeploy
	}
	if in.Build != nil {
		svc.Build = *in.Build
	}
	spec := &svc.Spec
	if in.Size != "" {
		size, ok := resources.SizeByName(in.Size)
		if !ok {
			return fmt.Errorf("unknown size %q", in.Size)
		}
		resources.Apply(spec, size, firstNonEmpty(in.QoS, spec.QoS))
	} else if in.QoS != "" {
		// Change only the QoS class, keeping the current limits.
		cur := spec.Resources
		size := resources.Size{MemoryLimitMB: cur.MemoryLimitMB, CPULimitMillis: cur.CPULimitMillis,
			MemoryRequestMB: max(cur.MemoryRequestMB, cur.MemoryLimitMB/3), CPURequestMillis: max(cur.CPURequestMillis, cur.CPULimitMillis/10)}
		if size.MemoryLimitMB == 0 {
			size, _ = resources.SizeByName("small")
		}
		resources.Apply(spec, size, in.QoS)
	}
	if in.Resources != nil {
		spec.Resources = *in.Resources
	}
	if in.Port != nil {
		if *in.Port < 0 || *in.Port > 65535 {
			return errors.New("invalid port")
		}
		spec.Port = *in.Port
	}
	if in.Replicas != nil {
		spec.Replicas = *in.Replicas
	}
	if in.Sleep != nil {
		spec.Sleep = *in.Sleep
	}
	if in.HealthCheck != nil {
		spec.HealthCheck = *in.HealthCheck
	}
	if in.RuntimeClass != nil {
		spec.RuntimeClass = *in.RuntimeClass
	}
	if in.Volumes != nil {
		for _, v := range *in.Volumes {
			if v.Name == "" || !strings.HasPrefix(v.Path, "/") || strings.Contains(v.Name, "/") || strings.Contains(v.Name, "..") {
				return errors.New("volumes need a name and an absolute path")
			}
		}
		spec.Volumes = *in.Volumes
	}
	if in.Command != nil {
		spec.Command = *in.Command
	}
	if in.SPA != nil {
		spec.SPA = *in.SPA
	}
	if in.Previews != nil {
		spec.Previews = *in.Previews
	}
	if in.Autoscale != nil {
		spec.Autoscale = *in.Autoscale
	}
	if in.Schedule != nil {
		spec.Schedule = strings.TrimSpace(*in.Schedule)
	}
	if in.JobTimeoutSec != nil {
		spec.JobTimeoutSec = *in.JobTimeoutSec
	}
	if svc.Type == model.TypeStatic {
		spec.Replicas = 0
		spec.Port = 0
	}
	if len(spec.Volumes) > 0 && spec.Replicas > 1 {
		return errors.New("services with volumes cannot run more than one replica")
	}
	if in.GitToken != nil {
		enc, err := s.box.Encrypt(*in.GitToken)
		if err != nil {
			return err
		}
		svc.GitTokenEnc = enc
	}
	return resources.Normalize(svc.Type, spec)
}

func (s *Server) handleCreateService(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "developer")
	if p == nil {
		return
	}
	var in serviceInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if in.Name == nil || in.Source == nil {
		writeError(w, 400, "name and source are required")
		return
	}
	switch in.Type {
	case "":
		in.Type = model.TypeWeb
	case model.TypeWeb, model.TypeWorker, model.TypeStatic, model.TypeCron:
	default:
		writeError(w, 400, "type must be web, worker, static or cron")
		return
	}
	if err := s.checkCounts(r, p.TeamID, 0, 1, 0); err != nil {
		writeError(w, 403, err.Error())
		return
	}
	svc := &store.Service{ProjectID: p.ID, Type: in.Type, WebhookSecret: util.RandomToken(20)}
	svc.Source.AutoDeploy = true
	if err := s.applyInput(svc, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.checkInstallation(r.Context(), p.TeamID, svc.Source.InstallationID); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.checkServiceSize(r, svc); err != nil {
		writeError(w, 403, err.Error())
		return
	}
	in.Domain = strings.ToLower(strings.TrimSpace(in.Domain))
	if in.Domain != "" {
		if err := s.checkCustomDomain(r, in.Domain); err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	svc.Slug = util.Slugify(svc.Name)
	for k := range in.Env {
		if !envKeyRe.MatchString(k) {
			writeError(w, 400, "invalid environment variable name "+k)
			return
		}
	}
	created, err := s.store.CreateService(r.Context(), svc)
	if err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, 409, "a service with this name already exists in the project")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	for k, v := range in.Env {
		_ = s.store.SetEnvVar(r.Context(), created.ID, k, s.box.MustEncrypt(v))
	}
	if created.Type != model.TypeWorker && created.Type != model.TypeCron {
		host := s.generatedDomain(created.Slug, p.Slug)
		if _, err := s.store.AddDomain(r.Context(), created.ID, host, true); err != nil {
			_, _ = s.store.AddDomain(r.Context(), created.ID, s.generatedDomain(created.Slug+"-"+util.RandomToken(2), p.Slug), true)
		}
		if in.Domain != "" {
			_, _ = s.store.AddDomain(r.Context(), created.ID, in.Domain, false)
		}
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "service.create", created.Slug)
	resp := map[string]any{"service": created}
	if in.Deploy == nil || *in.Deploy {
		d, err := s.QueueDeployment(r.Context(), created, "manual", "", "", &u.ID)
		if err == nil {
			resp["deployment"] = d
		}
	}
	writeJSON(w, 201, resp)
}

func (s *Server) webhookURL(svc *store.Service) string {
	return fmt.Sprintf("%s/api/v1/webhooks/github/%s", s.cfg.PublicURL, svc.ID)
}

func (s *Server) handleGetService(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	setETag(w, svc.Version)
	ctx := r.Context()
	domains, _ := s.store.DomainsForService(ctx, svc.ID)
	instances := s.instancesOf(ctx, svc.ID)
	deps, _ := s.store.DeploymentsForService(ctx, svc.ID, 10)
	links, _ := s.store.LinksForService(ctx, svc.ID)
	type linkOut struct {
		store.DatabaseLink
		Name   string `json:"name"`
		Engine string `json:"engine"`
	}
	linked := []linkOut{}
	for _, l := range links {
		if db, err := s.store.DatabaseByID(ctx, l.DatabaseID); err == nil {
			linked = append(linked, linkOut{DatabaseLink: l, Name: db.Name, Engine: db.Engine})
		}
	}
	peak := 0
	for _, in := range instances {
		if in.MemoryPeakMB > peak {
			peak = in.MemoryPeakMB
		}
	}
	nodes := map[string]string{}
	if ns, err := s.store.Nodes(ctx); err == nil {
		for _, n := range ns {
			nodes[n.ID] = n.Name
		}
	}
	type instOut struct {
		store.Instance
		NodeName string `json:"node_name"`
	}
	insts := []instOut{}
	for _, in := range instances {
		insts = append(insts, instOut{Instance: in, NodeName: nodes[in.NodeID]})
	}
	type previewOut struct {
		store.Service
		URL string `json:"url"`
	}
	previews := []previewOut{}
	if list, err := s.store.PreviewsOf(ctx, svc.ID); err == nil {
		for _, pv := range list {
			po := previewOut{Service: pv}
			if doms, _ := s.store.DomainsForService(ctx, pv.ID); len(doms) > 0 {
				po.URL = s.urlFor(doms[0].Hostname)
			}
			previews = append(previews, po)
		}
	}
	var nextRun *time.Time
	if svc.Type == model.TypeCron {
		if sch, err := cron.Parse(svc.Spec.Schedule); err == nil {
			n := sch.Next(time.Now().UTC())
			nextRun = &n
		}
	}
	role, _ := s.teamRole(r, p.TeamID)
	webhookSecret := svc.WebhookSecret
	if roleRank[role] < roleRank["developer"] {
		webhookSecret = ""
		if svc.Type == model.TypeDatabase {
			// Database servers carry credentials in their spec.
			svc.Spec.Env, svc.Spec.Command = nil, nil
		}
	}
	urls := []string{}
	for _, d := range domains {
		urls = append(urls, s.urlFor(d.Hostname))
	}
	if domains == nil {
		domains = []store.Domain{}
	}
	if deps == nil {
		deps = []store.Deployment{}
	}
	writeJSON(w, 200, map[string]any{
		"service":        svc,
		"project":        p,
		"domains":        domains,
		"urls":           urls,
		"instances":      insts,
		"deployments":    deps,
		"databases":      linked,
		"webhook_url":    s.webhookURL(svc),
		"webhook_secret": webhookSecret,
		"internal_host":  internalHost(svc.Slug, p.Slug),
		"recommendation": resources.Recommend(svc.Spec.Resources, peak),
		"tuning_env":     resources.TuningEnv(svc.Spec.Language, svc.Spec.Resources),
		"previews":       previews,
		"next_run":       nextRun,
	})
}

func (s *Server) handleUpdateService(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	var in serviceInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if in.Type != "" && in.Type != svc.Type {
		writeError(w, 400, "the service type cannot be changed")
		return
	}
	before := svc.Spec
	if err := s.applyInput(svc, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.checkInstallation(r.Context(), p.TeamID, svc.Source.InstallationID); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.checkServiceSize(r, svc); err != nil {
		writeError(w, 403, err.Error())
		return
	}
	// The checks above ran on the copy loaded for this request; the write re-applies
	// the input to the fresh row, so a concurrent change (a build recording the
	// language, a scale) to a field this request does not touch is kept.
	var inputErr error
	fresh, err := s.store.UpdateService(r.Context(), svc.ID, func(f *store.Service) error {
		if err := checkIfMatch(r, f.Version); err != nil {
			return err
		}
		before = f.Spec
		if inputErr = s.applyInput(f, &in); inputErr != nil {
			return inputErr
		}
		return nil
	})
	if inputErr != nil {
		writeError(w, 400, inputErr.Error())
		return
	}
	if err != nil {
		writeUpdateError(w, err)
		return
	}
	*svc = *fresh
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "service.update", svc.Slug)
	// Runtime settings take effect with a restart; replica changes converge without one.
	runtimeChanged := before.Resources != svc.Spec.Resources || before.Port != svc.Spec.Port || before.RuntimeClass != svc.Spec.RuntimeClass ||
		strings.Join(before.Command, " ") != strings.Join(svc.Spec.Command, " ") || before.HealthCheck != svc.Spec.HealthCheck
	resp := map[string]any{"service": svc}
	if runtimeChanged && svc.CurrentDeploymentID != nil && svc.Status == model.StatusRunning {
		if d, err := s.restartService(r.Context(), svc, &u.ID); err == nil {
			resp["deployment"] = d
		}
	} else if before.Replicas != svc.Spec.Replicas {
		go s.reconcileService(context.Background(), svc.ID)
	}
	if svc.Status == model.StatusSleeping && !svc.Spec.Sleep.Enabled {
		go s.wakeService(context.Background(), svc.ID)
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleDeleteService(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "admin")
	if svc == nil {
		return
	}
	if err := s.deleteService(context.WithoutCancel(r.Context()), svc); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "service.delete", svc.Slug)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	var in struct {
		CommitSHA string `json:"commit_sha"`
	}
	_ = decode(r, &in)
	if in.CommitSHA != "" && !validSHA(in.CommitSHA) {
		writeError(w, 400, "commit_sha must be a hex commit id")
		return
	}
	u := currentUser(r)
	d, err := s.QueueDeployment(r.Context(), svc, "manual", in.CommitSHA, "", &u.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 202, d)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	u := currentUser(r)
	d, err := s.restartService(r.Context(), svc, &u.ID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 202, d)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	s.stopService(context.WithoutCancel(r.Context()), svc)
	writeJSON(w, 200, map[string]string{"status": model.StatusStopped})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	if err := s.startService(context.WithoutCancel(r.Context()), svc); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": model.StatusRunning})
}

func (s *Server) handleScale(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	var in struct {
		Replicas int `json:"replicas"`
	}
	if err := decode(r, &in); err != nil || in.Replicas < 1 || in.Replicas > 50 {
		writeError(w, 400, "replicas must be between 1 and 50")
		return
	}
	if l := s.teamLimits(r.Context()); limited(r) && l.MaxReplicas > 0 && in.Replicas > l.MaxReplicas {
		writeError(w, 403, fmt.Sprintf("services are limited to %d replicas", l.MaxReplicas))
		return
	}
	if err := s.scaleService(r.Context(), svc, in.Replicas); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeUpdateError(w, err)
			return
		}
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int{"replicas": in.Replicas})
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	deps, err := s.store.DeploymentsForService(r.Context(), svc.ID, 50)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, deps)
}

// Environment variables.

func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	role, _ := s.teamRole(r, p.TeamID)
	masked := roleRank[role] < roleRank["developer"]
	vars, err := s.store.EnvVars(r.Context(), svc.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	out := map[string]string{}
	for _, v := range vars {
		plain, err := s.box.Decrypt(v.ValueEnc)
		if err != nil {
			plain = "<undecryptable>"
		}
		if masked {
			plain = "••••••"
		}
		out[v.Key] = plain
	}
	links, _ := s.store.LinksForService(r.Context(), svc.ID)
	linked := map[string]string{}
	for _, l := range links {
		linked[l.EnvName] = "(from linked database)"
	}
	writeJSON(w, 200, map[string]any{"vars": out, "linked": linked})
}

func (s *Server) handlePutEnv(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	var in struct {
		Vars    map[string]string `json:"vars"`
		Replace bool              `json:"replace"`
		Restart *bool             `json:"restart"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	for k := range in.Vars {
		if !envKeyRe.MatchString(k) {
			writeError(w, 400, "invalid environment variable name "+k)
			return
		}
	}
	if in.Replace {
		existing, _ := s.store.EnvVars(r.Context(), svc.ID)
		for _, e := range existing {
			if _, keep := in.Vars[e.Key]; !keep {
				_ = s.store.DeleteEnvVar(r.Context(), svc.ID, e.Key)
			}
		}
	}
	for k, v := range in.Vars {
		if err := s.store.SetEnvVar(r.Context(), svc.ID, k, s.box.MustEncrypt(v)); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	resp := map[string]any{"status": "saved"}
	if (in.Restart == nil || *in.Restart) && svc.CurrentDeploymentID != nil && svc.Status == model.StatusRunning {
		u := currentUser(r)
		if svc.Type == model.TypeStatic {
			// Static sites bake env vars in at build time.
			if d, err := s.QueueDeployment(r.Context(), svc, "env", "", "", &u.ID); err == nil {
				resp["deployment"] = d
			}
		} else if d, err := s.restartService(r.Context(), svc, &u.ID); err == nil {
			resp["deployment"] = d
		}
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleDeleteEnv(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	if err := s.store.DeleteEnvVar(r.Context(), svc.ID, r.PathValue("key")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// Domains.

var hostnameRe = regexp.MustCompile(`^(?i)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$`)

func validHostname(h string) error {
	if !hostnameRe.MatchString(h) || len(h) > 253 {
		return fmt.Errorf("invalid hostname %q", h)
	}
	return nil
}

// checkCustomDomain validates a hostname a user asks to route to a service. The
// dashboard and database hosts are never available; names under the base domain
// are reserved for generated domains unless the caller is a system administrator.
func (s *Server) checkCustomDomain(r *http.Request, host string) error {
	if err := validHostname(host); err != nil {
		return err
	}
	if host == s.cfg.DashboardDomain || host == s.cfg.DBHost {
		return fmt.Errorf("hostname %q is reserved", host)
	}
	if (host == s.cfg.BaseDomain || strings.HasSuffix(host, "."+s.cfg.BaseDomain)) && !currentUser(r).IsAdmin {
		return fmt.Errorf("hostnames under the platform domain are reserved")
	}
	return nil
}

func (s *Server) handleListDomains(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	d, err := s.store.DomainsForService(r.Context(), svc.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, d)
}

func (s *Server) handleAddDomain(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	var in struct {
		Hostname string `json:"hostname"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	host := strings.ToLower(strings.TrimSpace(in.Hostname))
	if err := s.checkCustomDomain(r, host); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	d, err := s.store.AddDomain(r.Context(), svc.ID, host, false)
	if err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, 409, "domain already in use")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	s.publishRoutes(r.Context())
	writeJSON(w, 201, d)
}

func (s *Server) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	if err := s.store.DeleteDomain(r.Context(), svc.ID, r.PathValue("domain")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.publishRoutes(r.Context())
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// Runtime logs and exec.

func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if tail <= 0 || tail > 5000 {
		tail = 300
	}
	type instLogs struct {
		Instance string   `json:"instance"`
		Lines    []string `json:"lines"`
		Error    string   `json:"error,omitempty"`
	}
	out := []instLogs{}
	for _, in := range s.instancesOf(r.Context(), svc.ID) {
		var rep proto.LogsReply
		err := s.requestAgent(r.Context(), in.NodeID, proto.CmdLogs, proto.LogsRequest{ContainerID: in.ContainerID, Tail: tail}, &rep, 10*time.Second)
		il := instLogs{Instance: in.ContainerID, Lines: rep.Lines, Error: rep.Error}
		if err != nil {
			il.Error = err.Error()
		}
		if il.Lines == nil {
			il.Lines = []string{}
		}
		out = append(out, il)
	}
	writeJSON(w, 200, out)
}

// handleServiceLogStream streams live runtime logs from every instance via SSE.
func (s *Server) handleServiceLogStream(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	lines := make(chan string, 512)
	streamID := util.RandomToken(8)
	subject := "paas.logs.live." + streamID
	sub, err := s.nc.Subscribe(subject+".*", func(m *nats.Msg) {
		prefix := strings.TrimPrefix(m.Subject, subject+".")
		select {
		case lines <- prefix + " | " + string(m.Data):
		default:
		}
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer sub.Unsubscribe()
	stopSubject := "paas.logs.stop." + streamID
	defer s.nc.Publish(stopSubject, nil)
	for _, in := range s.instancesOf(r.Context(), svc.ID) {
		var rep proto.LogsReply
		short := in.ContainerID
		if len(short) > 6 {
			short = short[len(short)-6:]
		}
		_ = s.requestAgent(r.Context(), in.NodeID, proto.CmdLogs, proto.LogsRequest{
			ContainerID: in.ContainerID, Tail: 100, Follow: true, Subject: subject + "." + short, StopSubject: stopSubject,
		}, &rep, 10*time.Second)
		for _, l := range rep.Lines {
			writeSSE(w, "log", short+" | "+l)
		}
	}
	flusher.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case l := <-lines:
			writeSSE(w, "log", l)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\n", event)
	for _, l := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", l)
	}
	fmt.Fprint(w, "\n")
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	var in struct {
		Command  string `json:"command"`
		Instance string `json:"instance"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Command) == "" {
		writeError(w, 400, "command required")
		return
	}
	var target *store.Instance
	for _, inst := range s.instancesOf(r.Context(), svc.ID) {
		if inst.Status == model.InstRunning && (in.Instance == "" || inst.ContainerID == in.Instance) {
			target = &inst
			break
		}
	}
	if target == nil {
		writeError(w, 400, "no running instance")
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "service.exec", svc.Slug+": "+in.Command)
	var rep proto.ExecReply
	err := s.requestAgent(r.Context(), target.NodeID, proto.CmdExec, proto.ExecRequest{ContainerID: target.ContainerID, Cmd: []string{"/bin/sh", "-c", in.Command}, TimeoutSec: 120}, &rep, 130*time.Second)
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, rep)
}

// Deployments.

func (s *Server) deploymentFor(w http.ResponseWriter, r *http.Request, min string) (*store.Deployment, *store.Service) {
	d, err := s.store.DeploymentByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "deployment not found")
		return nil, nil
	}
	svc, err := s.store.ServiceByID(r.Context(), d.ServiceID)
	if err != nil {
		writeError(w, 404, "service not found")
		return nil, nil
	}
	if s.projectFor(w, r, svc.ProjectID, min) == nil {
		return nil, nil
	}
	return d, svc
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	d, svc := s.deploymentFor(w, r, "viewer")
	if d == nil {
		return
	}
	writeJSON(w, 200, map[string]any{"deployment": d, "service": svc})
}

func (s *Server) handleDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	d, _ := s.deploymentFor(w, r, "viewer")
	if d == nil {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	lines, err := s.store.DeploymentLogs(r.Context(), d.ID, after, 5000)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if lines == nil {
		lines = []store.LogLine{}
	}
	writeJSON(w, 200, lines)
}

// handleDeploymentLogStream replays stored lines then streams new ones until the deployment finishes.
func (s *Server) handleDeploymentLogStream(w http.ResponseWriter, r *http.Request) {
	d, _ := s.deploymentFor(w, r, "viewer")
	if d == nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, cancel := s.hub.Subscribe(d.ID)
	defer cancel()
	var last int64
	if v, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil {
		last = v // EventSource reconnect: continue where the client stopped
	}
	sendStored := func() {
		lines, _ := s.store.DeploymentLogs(r.Context(), d.ID, last, 5000)
		for _, l := range lines {
			fmt.Fprintf(w, "id: %d\n", l.ID)
			writeSSE(w, "log", l.Line)
			last = l.ID
		}
	}
	sendStored()
	flusher.Flush()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			// Lines are persisted before fan-out, so read them back in order.
			sendStored()
			flusher.Flush()
		case <-t.C:
			cur, err := s.store.DeploymentByID(r.Context(), d.ID)
			if err != nil {
				return
			}
			switch cur.Status {
			case model.DeployActive, model.DeployFailed, model.DeployCancelled, model.DeploySuperseded:
				time.Sleep(300 * time.Millisecond)
				sendStored()
				writeSSE(w, "done", cur.Status)
				flusher.Flush()
				return
			}
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	d, svc := s.deploymentFor(w, r, "developer")
	if d == nil {
		return
	}
	u := currentUser(r)
	nd, err := s.redeploy(r.Context(), svc, d, "rollback", &u.ID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 202, nd)
}

func (s *Server) handleCancelDeployment(w http.ResponseWriter, r *http.Request) {
	d, _ := s.deploymentFor(w, r, "developer")
	if d == nil {
		return
	}
	if d.Status != model.DeployQueued {
		writeError(w, 400, "only queued deployments can be cancelled")
		return
	}
	_ = s.store.SetDeploymentStatus(r.Context(), d.ID, model.DeployCancelled, "cancelled by user")
	writeJSON(w, 200, map[string]string{"status": model.DeployCancelled})
}

func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	if svc.Type != model.TypeCron {
		writeError(w, 400, "only cron services can be run on demand")
		return
	}
	run, err := s.runJob(context.WithoutCancel(r.Context()), svc, "manual", false)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 202, run)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	runs, err := s.store.JobRuns(r.Context(), svc.ID, 50)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if runs == nil {
		runs = []store.JobRun{}
	}
	writeJSON(w, 200, runs)
}
