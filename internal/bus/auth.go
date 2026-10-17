package bus

import (
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"math"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"
)

// NodePermissions maps a node identity and its roles to NATS permissions.
// Every joined node runs the agent, so the agent grants apply to all roles; edge adds the proxy subjects.
func NodePermissions(nodeID string, roles []string) *server.Permissions {
	sub := []string{"_INBOX." + nodeID + ".>", "paas.agent." + nodeID + ".>", "paas.dns", "paas.mesh", "paas.logs.stop.*"}
	pub := []string{"paas.node." + nodeID + ".>", "paas.agent.binary", "paas.dns.get", "paas.mesh.get", "paas.logs.deploy.*", "paas.logs.live.*.*"}
	edge := false
	for _, r := range roles {
		edge = edge || r == "edge"
	}
	if edge {
		sub = append(sub, "paas.proxy.routes")
		pub = append(pub, "paas.proxy.routes.get", "paas.proxy.stats", "paas.proxy.wake")
	}
	return &server.Permissions{
		Publish:   &server.SubjectPermission{Allow: pub},
		Subscribe: &server.SubjectPermission{Allow: sub},
		Response:  &server.ResponsePermission{MaxMsgs: 1},
	}
}

// Auth authenticates NATS clients. Routes do not pass through it.
type Auth struct {
	Internal string // in-process secret: full permissions
	// Lookup resolves a node public key to its identity; called on every connect, never cached.
	Lookup func(nkey string) (nodeID string, roles []string, ok bool)
	Legacy string // shared token accepted with full permissions while non-empty
}

var _ server.Authentication = (*Auth)(nil)

// Check implements server.Authentication.
func (a *Auth) Check(c server.ClientAuthentication) bool {
	if c.Kind() != server.CLIENT {
		return false
	}
	o := c.GetOpts()
	if o.Nkey != "" {
		return a.checkNkey(c, o.Nkey, o.Sig)
	}
	if a.Internal != "" && subtle.ConstantTimeCompare([]byte(o.Token), []byte(a.Internal)) == 1 {
		c.RegisterUser(&server.User{Username: "internal"})
		return true
	}
	// Legacy: remove when no node has nkey IS NULL (plan 261017-cluster-trust).
	if a.Legacy != "" && subtle.ConstantTimeCompare([]byte(o.Token), []byte(a.Legacy)) == 1 {
		slog.Warn("nats client authenticated with legacy shared token", "addr", c.RemoteAddress())
		c.RegisterUser(&server.User{Username: "legacy"})
		return true
	}
	return false
}

func (a *Auth) checkNkey(c server.ClientAuthentication, nkey, sig string) bool {
	if sig == "" || a.Lookup == nil {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(sig); err != nil {
			return false
		}
	}
	pub, err := nkeys.FromPublicKey(nkey)
	if err != nil || pub.Verify(c.GetNonce(), raw) != nil {
		return false
	}
	nodeID, roles, ok := a.Lookup(nkey)
	if !ok {
		return false
	}
	c.RegisterUser(&server.User{Username: nkey, Permissions: NodePermissions(nodeID, roles)})
	return true
}

// Kick disconnects every live connection authenticated as nkey (checkNkey registers it as the username).
func (a *Auth) Kick(ns *server.Server, nkey string) {
	cz, err := ns.Connz(&server.ConnzOptions{Username: true, State: server.ConnOpen, Limit: math.MaxInt32})
	if err != nil {
		slog.Warn("kick: list connections", "err", err)
		return
	}
	for _, c := range cz.Conns {
		if c.AuthorizedUser == nkey {
			_ = ns.DisconnectClientByID(c.Cid)
		}
	}
}
