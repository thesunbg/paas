package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// meshTable lists every node's WireGuard identity and endpoint.
func (s *Server) meshTable(ctx context.Context) (*proto.MeshTable, error) {
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	t := &proto.MeshTable{}
	for _, n := range nodes {
		if n.NetIndex == nil || n.WGPublicKey == "" || n.Status == "offline" {
			continue
		}
		t.Peers = append(t.Peers, proto.MeshPeer{
			NodeID: n.ID, NodeIndex: *n.NetIndex, PublicKey: n.WGPublicKey,
			Endpoint: net.JoinHostPort(n.Address, strconv.Itoa(n.WGPort)),
		})
	}
	return t, nil
}

func (s *Server) publishMesh(ctx context.Context) {
	t, err := s.meshTable(ctx)
	if err != nil {
		slog.Warn("mesh table", "err", err)
		return
	}
	_ = bus.Publish(s.nc, proto.SubjMesh, t)
}

// nodeMeshAddr returns host:port of a service exposed by a node's agent on its mesh address.
func (s *Server) nodeMeshAddr(n *store.Node, port int) string {
	if n == nil || n.NetIndex == nil {
		return ""
	}
	return net.JoinHostPort(network.NodeMeshIP(s.clusterBase, *n.NetIndex), strconv.Itoa(port))
}

// StaticOriginPort is where agents with the build role serve static outputs over the mesh.
const StaticOriginPort = 7070

const registrySlug = "registry"

// ensureRegistry starts the internal image registry (used to move images between nodes)
// and returns its address.
func (s *Server) ensureRegistry(ctx context.Context, pinNode string) (string, error) {
	reg, err := s.registryService(ctx, pinNode)
	if err != nil {
		return "", err
	}
	if err := s.waitServiceRunning(ctx, reg.ID, 5*time.Minute); err != nil {
		return "", fmt.Errorf("internal registry: %w", err)
	}
	in, err := s.firstInstance(ctx, reg.ID)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(in.IP, "5000"), nil
}

func (s *Server) registryService(ctx context.Context, pinNode string) (*store.Service, error) {
	unlock := s.lock("system-registry")
	defer unlock()
	services, err := s.store.ServicesForProject(ctx, s.systemProjectID)
	if err != nil {
		return nil, err
	}
	for i := range services {
		if services[i].Slug != registrySlug {
			continue
		}
		reg := &services[i]
		if reg.Status == model.StatusFailed || reg.Status == model.StatusStopped || reg.CurrentDeploymentID == nil && reg.Status != model.StatusDeploying {
			if _, err := s.QueueDeployment(ctx, reg, "system", "", "", nil); err != nil {
				return nil, err
			}
			_ = s.store.SetServiceStatus(ctx, reg.ID, model.StatusDeploying)
		}
		return reg, nil
	}
	spec := model.ServiceSpec{
		Port: 5000, Replicas: 1, Image: "docker.io/library/registry:2",
		Resources: model.Resources{MemoryLimitMB: 256, MemoryRequestMB: 64, CPULimitMillis: 1000, CPURequestMillis: 50},
		QoS:       model.QoSBurstable, HealthCheck: model.HealthCheck{Type: "tcp", TimeoutSec: 60},
		Volumes: []model.Volume{{Name: "data", Path: "/var/lib/registry"}},
		Env:     map[string]string{"REGISTRY_STORAGE_DELETE_ENABLED": "true"},
	}
	reg, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: s.systemProjectID, Name: "Image registry", Slug: registrySlug, Type: model.TypeDatabase,
		Source: model.ServiceSource{Kind: "image", Image: spec.Image}, Spec: spec, WebhookSecret: util.RandomToken(16), NodeID: &pinNode,
	})
	if err != nil {
		return nil, err
	}
	slog.Info("starting internal image registry", "node", pinNode)
	if _, err := s.QueueDeployment(ctx, reg, "system", "", "", nil); err != nil {
		return nil, err
	}
	_ = s.store.SetServiceStatus(ctx, reg.ID, model.StatusDeploying)
	return reg, nil
}

// imageFor returns the image reference a node should run for a deployment,
// pushing locally built images to the internal registry when the node is not the build node.
func (s *Server) imageFor(ctx context.Context, d *store.Deployment, node *store.Node, logID string) (ref string, plainRegistry string, err error) {
	if !strings.HasPrefix(d.Image, "paas.local/") || d.BuildNodeID == nil || *d.BuildNodeID == node.ID {
		return d.Image, "", nil
	}
	reg, err := s.ensureRegistry(ctx, *d.BuildNodeID)
	if err != nil {
		return "", "", err
	}
	target := reg + "/" + strings.TrimPrefix(d.Image, "paas.local/")
	if d.PushedImage != target {
		if logID != "" {
			s.hub.Log(logID, "==> pushing image to the internal registry for node "+node.Name)
		}
		var rep proto.GenericReply
		if err := s.requestAgent(ctx, *d.BuildNodeID, proto.CmdPush, proto.PushRequest{Image: d.Image, Target: target, Insecure: true}, &rep, 15*time.Minute); err != nil {
			return "", "", fmt.Errorf("push image: %w", err)
		}
		if rep.Error != "" {
			return "", "", errors.New("push image: " + rep.Error)
		}
		_ = s.store.SetDeploymentPushed(ctx, d.ID, target)
		d.PushedImage = target
	}
	return target, reg, nil
}
