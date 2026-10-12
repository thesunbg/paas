package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/resources"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "viewer") {
		return
	}
	projects, err := s.store.ProjectsForTeam(r.Context(), team)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	type item struct {
		store.Project
		Services  int `json:"services"`
		Databases int `json:"databases"`
		Running   int `json:"running"`
	}
	out := make([]item, 0, len(projects))
	for _, p := range projects {
		it := item{Project: p}
		svcs, _ := s.store.ServicesForProject(r.Context(), p.ID)
		for _, sv := range svcs {
			if sv.Type == model.TypeDatabase {
				continue
			}
			it.Services++
			if sv.Status == model.StatusRunning {
				it.Running++
			}
		}
		dbs, _ := s.store.DatabasesForProject(r.Context(), p.ID)
		it.Databases = len(dbs)
		out = append(out, it)
	}
	writeJSON(w, 200, out)
}

// uniqueProjectSlug keeps project slugs globally unique so internal DNS names never collide.
func (s *Server) uniqueProjectSlug(ctx context.Context, name string) string {
	base := util.Slugify(name)
	if base == systemProjectSlug || base == "shared" {
		base += "-app"
	}
	slug := base
	for i := 0; i < 20; i++ {
		var exists bool
		_ = s.store.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE slug=$1)`, slug).Scan(&exists)
		if !exists {
			return slug
		}
		slug = base + "-" + util.RandomToken(2)
	}
	return slug
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "developer") {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeError(w, 400, "name required")
		return
	}
	p, err := s.store.CreateProject(r.Context(), team, strings.TrimSpace(in.Name), s.uniqueProjectSlug(r.Context(), in.Name))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &team, "project.create", p.Slug)
	writeJSON(w, 201, p)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "viewer")
	if p == nil {
		return
	}
	svcs, err := s.store.ServicesForProject(r.Context(), p.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	type svcItem struct {
		store.Service
		URL       string `json:"url,omitempty"`
		Instances int    `json:"instances"`
		MemoryMB  int    `json:"memory_mb"`
	}
	services := []svcItem{}
	for _, sv := range svcs {
		if sv.Type == model.TypeDatabase {
			continue
		}
		it := svcItem{Service: sv}
		if doms, _ := s.store.DomainsForService(r.Context(), sv.ID); len(doms) > 0 {
			it.URL = s.urlFor(doms[0].Hostname)
		}
		for _, in := range s.instancesOf(r.Context(), sv.ID) {
			if in.Status == model.InstRunning {
				it.Instances++
				it.MemoryMB += in.MemoryUsageMB
			}
		}
		services = append(services, it)
	}
	dbs, _ := s.store.DatabasesForProject(r.Context(), p.ID)
	if dbs == nil {
		dbs = []store.Database{}
	}
	writeJSON(w, 200, map[string]any{"project": p, "services": services, "databases": dbs})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "admin")
	if p == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	dbs, _ := s.store.DatabasesForProject(ctx, p.ID)
	for i := range dbs {
		_ = s.deleteDatabase(ctx, &dbs[i])
	}
	svcs, _ := s.store.ServicesForProject(ctx, p.ID)
	for i := range svcs {
		_ = s.deleteService(ctx, &svcs[i])
	}
	if err := s.store.DeleteProject(ctx, p.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, &p.TeamID, "project.delete", p.Slug)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) handleSizes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"sizes": resources.Sizes, "database_sizes": databases.Presets, "engines": databases.Engines})
}

// handleOverview returns cluster-wide numbers for the dashboard home page.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(r)
	teams, _ := s.store.TeamsForUser(ctx, u.ID)
	var projects, services, running, sleeping, databases int
	for _, t := range teams {
		ps, _ := s.store.ProjectsForTeam(ctx, t.ID)
		projects += len(ps)
		for _, p := range ps {
			svcs, _ := s.store.ServicesForProject(ctx, p.ID)
			for _, sv := range svcs {
				if sv.Type == model.TypeDatabase {
					continue
				}
				services++
				switch sv.Status {
				case model.StatusRunning:
					running++
				case model.StatusSleeping:
					sleeping++
				}
			}
			dbs, _ := s.store.DatabasesForProject(ctx, p.ID)
			databases += len(dbs)
		}
	}
	out := map[string]any{"projects": projects, "services": services, "running": running, "sleeping": sleeping, "databases": databases}
	if u.IsAdmin {
		nodes, _ := s.store.Nodes(ctx)
		out["nodes"] = len(nodes)
	}
	writeJSON(w, 200, out)
}
