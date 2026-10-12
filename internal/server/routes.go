package server

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

// routeTable computes what every hostname should serve.
func (s *Server) routeTable(ctx context.Context) (*model.RouteTable, error) {
	services, err := s.store.AllServices(ctx)
	if err != nil {
		return nil, err
	}
	domains, err := s.store.AllDomains(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := s.store.AllInstances(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]*store.Service{}
	for i := range services {
		byID[services[i].ID] = &services[i]
	}
	upstreams := map[string][]string{}
	for _, in := range instances {
		svc := byID[in.ServiceID]
		if svc == nil || in.Status != model.InstRunning || in.IP == "" || in.Port == 0 {
			continue
		}
		if svc.CurrentDeploymentID == nil || in.DeploymentID == nil || *in.DeploymentID != *svc.CurrentDeploymentID {
			continue
		}
		upstreams[in.ServiceID] = append(upstreams[in.ServiceID], net.JoinHostPort(in.IP, strconv.Itoa(in.Port)))
	}
	nodes, _ := s.store.Nodes(ctx)
	nodeByID := map[string]*store.Node{}
	for i := range nodes {
		nodeByID[nodes[i].ID] = &nodes[i]
	}
	staticPaths := map[string]string{}
	staticOrigins := map[string]string{}
	for _, svc := range services {
		if svc.CurrentDeploymentID == nil {
			continue
		}
		if svc.Type == model.TypeStatic || svc.Type == model.TypeWeb {
			d, err := s.store.DeploymentByID(ctx, *svc.CurrentDeploymentID)
			if err == nil && d.StaticPath != "" {
				staticPaths[svc.ID] = d.StaticPath
				if d.BuildNodeID != nil {
					if addr := s.nodeMeshAddr(nodeByID[*d.BuildNodeID], StaticOriginPort); addr != "" {
						staticOrigins[svc.ID] = "http://" + addr
					}
				}
			}
		}
	}

	s.routesMu.Lock()
	s.routesVersion++
	version := s.routesVersion
	s.routesMu.Unlock()
	t := &model.RouteTable{Version: version, Generated: time.Now()}
	if s.cfg.DashboardDomain != "" {
		t.Routes = append(t.Routes, model.Route{Host: s.cfg.DashboardDomain, Kind: "dashboard"})
	}
	for _, d := range domains {
		svc := byID[d.ServiceID]
		if svc == nil {
			continue
		}
		r := model.Route{Host: d.Hostname, ServiceID: svc.ID}
		switch {
		case staticPaths[svc.ID] != "":
			r.Kind = "static"
			r.StaticRoot = staticPaths[svc.ID]
			r.StaticOrigin = staticOrigins[svc.ID]
			r.SPA = svc.Spec.SPA || svc.Type == model.TypeStatic
		case svc.Status == model.StatusSleeping:
			r.Kind = "sleeping"
		case len(upstreams[svc.ID]) > 0:
			r.Kind = "upstream"
			r.Upstreams = upstreams[svc.ID]
		default:
			r.Kind = "unavailable"
		}
		t.Routes = append(t.Routes, r)
	}
	return t, nil
}

func (s *Server) publishRoutes(ctx context.Context) {
	t, err := s.routeTable(ctx)
	if err != nil {
		slog.Warn("route table", "err", err)
		return
	}
	_ = bus.Publish(s.nc, proto.SubjRoutes, t)
}

// dnsTable maps <service>.<project>.internal to the IPs of running instances.
func (s *Server) dnsTable(ctx context.Context) (*model.DNSTable, error) {
	rows, err := s.store.DB.Query(ctx, `SELECT s.slug, p.slug, i.ip FROM instances i
		JOIN services s ON s.id=i.service_id JOIN projects p ON p.id=s.project_id
		WHERE i.status='running' AND i.ip<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &model.DNSTable{Records: map[string][]string{}}
	for rows.Next() {
		var svc, proj, ip string
		if err := rows.Scan(&svc, &proj, &ip); err != nil {
			return nil, err
		}
		name := internalHost(svc, proj)
		t.Records[name] = append(t.Records[name], ip)
	}
	return t, rows.Err()
}

func (s *Server) publishDNS(ctx context.Context) {
	t, err := s.dnsTable(ctx)
	if err != nil {
		slog.Warn("dns table", "err", err)
		return
	}
	_ = bus.Publish(s.nc, proto.SubjDNS, t)
}

// publishAll pushes routes and DNS after a topology change.
func (s *Server) publishAll(ctx context.Context) {
	s.publishDNS(ctx)
	s.publishRoutes(ctx)
}
