package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/thesunbg/paas/internal/store"
)

// Notification event names.
var notificationEvents = []string{"deploy.succeeded", "deploy.failed", "instance.crashloop", "instance.evicted", "node.offline", "backup.failed", "job.failed", "autoscale", "database.quota", "database.failover"}

var notifyClient = &http.Client{Timeout: 10 * time.Second}

// AllowPrivateNotifyTargets disables the SSRF guard (tests / trusted networks).
var AllowPrivateNotifyTargets = os.Getenv("PAAS_NOTIFY_ALLOW_PRIVATE") == "true"

func init() {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		if AllowPrivateNotifyTargets {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() || cgnat.Contains(ip) {
			return fmt.Errorf("notification target %s is not a public address", host)
		}
		return nil
	}}
	notifyClient.Transport = &http.Transport{DialContext: dialer.DialContext, Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 5 * time.Second}
}

var _, cgnat, _ = net.ParseCIDR("100.64.0.0/10")

// notify delivers an event to the team's channels that subscribed to it (best effort, async).
func (s *Server) notify(ctx context.Context, teamID, event, text string, data map[string]any) {
	channels, err := s.store.Channels(ctx, teamID)
	if err != nil {
		return
	}
	for _, c := range channels {
		if !slices.Contains(c.Events, event) {
			continue
		}
		go func(c store.NotificationChannel) {
			if err := s.send(c, event, text, data); err != nil {
				slog.Warn("notification failed", "channel", c.Name, "event", event, "err", err)
			}
		}(c)
	}
}

// notifyAdmins sends cluster-level events to every administrator's team.
func (s *Server) notifyAdmins(ctx context.Context, event, text string, data map[string]any) {
	teams, err := s.store.AdminTeamIDs(ctx)
	if err != nil {
		return
	}
	for _, t := range teams {
		s.notify(ctx, t, event, text, data)
	}
}

func (s *Server) send(c store.NotificationChannel, event, text string, data map[string]any) error {
	target, err := s.box.Decrypt(c.TargetEnc)
	if err != nil {
		return err
	}
	icon := "ℹ️"
	switch {
	case strings.HasSuffix(event, "failed") || event == "node.offline" || event == "instance.crashloop" || event == "database.failover" || event == "database.quota":
		icon = "🔴"
	case strings.HasSuffix(event, "succeeded"):
		icon = "✅"
	}
	msg := icon + " " + text
	var url string
	var payload any
	switch c.Kind {
	case "slack":
		url, payload = target, map[string]string{"text": msg}
	case "discord":
		url, payload = target, map[string]string{"content": msg}
	case "telegram":
		// target: <bot token>:<chat id>, e.g. 123456:ABC-DEF:987654321
		i := strings.LastIndex(target, ":")
		if i <= 0 {
			return fmt.Errorf("telegram target must be <bot token>:<chat id>")
		}
		url = "https://api.telegram.org/bot" + target[:i] + "/sendMessage"
		payload = map[string]string{"chat_id": target[i+1:], "text": msg}
	default:
		url, payload = target, map[string]any{"event": event, "text": text, "data": data, "time": time.Now().UTC()}
	}
	body, _ := json.Marshal(payload)
	resp, err := notifyClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// teamOfService finds the team owning a service, for routing notifications.
func (s *Server) teamOfService(ctx context.Context, svc *store.Service) string {
	p, err := s.store.ProjectByID(ctx, svc.ProjectID)
	if err != nil {
		return ""
	}
	return p.TeamID
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "viewer") {
		return
	}
	ch, err := s.store.Channels(r.Context(), team)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if ch == nil {
		ch = []store.NotificationChannel{}
	}
	writeJSON(w, 200, map[string]any{"channels": ch, "events": notificationEvents})
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "admin") {
		return
	}
	var in struct {
		Name   string   `json:"name"`
		Kind   string   `json:"kind"`
		Target string   `json:"target"`
		Events []string `json:"events"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	switch in.Kind {
	case "webhook", "slack", "discord":
		if !strings.HasPrefix(in.Target, "https://") && !strings.HasPrefix(in.Target, "http://") {
			writeError(w, 400, "target must be a URL")
			return
		}
	case "telegram":
		if strings.Count(in.Target, ":") < 2 {
			writeError(w, 400, "telegram target must be <bot token>:<chat id>")
			return
		}
	default:
		writeError(w, 400, "kind must be webhook, slack, discord or telegram")
		return
	}
	if len(in.Events) == 0 {
		in.Events = []string{"deploy.failed", "instance.crashloop", "node.offline", "backup.failed", "job.failed", "database.quota", "database.failover"}
	}
	for _, e := range in.Events {
		if !slices.Contains(notificationEvents, e) {
			writeError(w, 400, "unknown event "+e)
			return
		}
	}
	if in.Name == "" {
		in.Name = in.Kind
	}
	c, err := s.store.CreateChannel(r.Context(), &store.NotificationChannel{TeamID: team, Name: in.Name, Kind: in.Kind, TargetEnc: s.box.MustEncrypt(in.Target), Events: in.Events})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, c)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "admin") {
		return
	}
	if err := s.store.DeleteChannel(r.Context(), team, r.PathValue("channel")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "admin") {
		return
	}
	chs, _ := s.store.Channels(r.Context(), team)
	for _, c := range chs {
		if c.ID == r.PathValue("channel") {
			if err := s.send(c, "test", "Test notification from paas", nil); err != nil {
				writeError(w, 502, err.Error())
				return
			}
			writeJSON(w, 200, map[string]string{"status": "sent"})
			return
		}
	}
	writeError(w, 404, "channel not found")
}
