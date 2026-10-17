package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thesunbg/paas/internal/proto"
)

type Node struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Address          string            `json:"address"`
	Roles            []string          `json:"roles"`
	Labels           map[string]string `json:"labels"`
	Status           string            `json:"status"`
	CPUCores         int               `json:"cpu_cores"`
	MemoryMB         int               `json:"memory_mb"`
	ReservedMB       int               `json:"reserved_mb"`
	OvercommitMemory float64           `json:"overcommit_memory"`
	OvercommitCPU    float64           `json:"overcommit_cpu"`
	Capabilities     map[string]string `json:"capabilities"`
	Usage            proto.NodeUsage   `json:"usage"`
	Version          string            `json:"version"`
	NetIndex         *int              `json:"net_index"`
	WGPublicKey      string            `json:"wg_public_key"`
	WGPort           int               `json:"wg_port"`
	LastSeenAt       time.Time         `json:"last_seen_at"`
	CreatedAt        time.Time         `json:"created_at"`
}

func (n *Node) HasRole(role string) bool {
	for _, r := range n.Roles {
		if r == role {
			return true
		}
	}
	return false
}

const nodeCols = `id, name, address, roles, labels, status, cpu_cores, memory_mb, reserved_mb, overcommit_memory, overcommit_cpu, capabilities, usage, version, net_index, wg_public_key, wg_port, last_seen_at, created_at`

func scanNode(row pgx.Row) (*Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.Name, &n.Address, &n.Roles, &n.Labels, &n.Status, &n.CPUCores, &n.MemoryMB, &n.ReservedMB,
		&n.OvercommitMemory, &n.OvercommitCPU, &n.Capabilities, &n.Usage, &n.Version, &n.NetIndex, &n.WGPublicKey, &n.WGPort, &n.LastSeenAt, &n.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &n, nil
}

// UpsertNode registers a node by name, keeping operator-set fields (status, overcommit) on re-registration.
// It also allocates the node's network index (its slice of the cluster address space).
func (s *Store) UpsertNode(ctx context.Context, r *proto.RegisterRequest, maxNodes int) (*Node, error) {
	var n *Node
	err := pgx.BeginFunc(ctx, s.q(ctx), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE nodes IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var err error
		n, err = scanNode(tx.QueryRow(ctx, `INSERT INTO nodes (name, address, roles, cpu_cores, memory_mb, capabilities, version, wg_public_key, wg_port, status, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'online',now())
		ON CONFLICT (name) DO UPDATE SET address=EXCLUDED.address, roles=EXCLUDED.roles, cpu_cores=EXCLUDED.cpu_cores,
			memory_mb=EXCLUDED.memory_mb, capabilities=EXCLUDED.capabilities, version=EXCLUDED.version,
			wg_public_key=EXCLUDED.wg_public_key, wg_port=EXCLUDED.wg_port, last_seen_at=now(),
			status = CASE WHEN nodes.status='cordoned' THEN 'cordoned' ELSE 'online' END
		RETURNING `+nodeCols, r.Name, r.Address, r.Roles, r.CPUCores, r.MemoryMB, r.Capabilities, r.Version, r.WGPublicKey, r.WGPort))
		if err != nil || n.NetIndex != nil {
			return err
		}
		n, err = scanNode(tx.QueryRow(ctx, `UPDATE nodes SET net_index = (
			SELECT min(i) FROM generate_series(1, $2::int) i WHERE i NOT IN (SELECT net_index FROM nodes WHERE net_index IS NOT NULL))
			WHERE id=$1 RETURNING `+nodeCols, n.ID, maxNodes))
		if err == nil && n.NetIndex == nil {
			return fmt.Errorf("cluster is full: no free node network index")
		}
		return err
	})
	return n, err
}

func (s *Store) NodeByID(ctx context.Context, id string) (*Node, error) {
	return scanNode(s.q(ctx).QueryRow(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id=$1`, id))
}

func (s *Store) Nodes(ctx context.Context) ([]Node, error) {
	return queryNodes(ctx, s.q(ctx))
}

func queryNodes(ctx context.Context, q Querier) ([]Node, error) {
	rows, err := q.Query(ctx, `SELECT `+nodeCols+` FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Node, error) {
		n, err := scanNode(r)
		if err != nil {
			return Node{}, err
		}
		return *n, nil
	})
}

func (s *Store) NodeHeartbeat(ctx context.Context, id string, usage proto.NodeUsage) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE nodes SET usage=$2, last_seen_at=now(),
		status = CASE WHEN status='offline' THEN 'online' ELSE status END WHERE id=$1`, id, usage)
	return err
}

// MarkStaleNodesOffline flags nodes without a heartbeat for longer than d and returns their ids.
func (s *Store) MarkStaleNodesOffline(ctx context.Context, d time.Duration) ([]string, error) {
	rows, err := s.q(ctx).Query(ctx, `UPDATE nodes SET status='offline' WHERE status='online' AND last_seen_at < now() - $1::interval RETURNING id`,
		d.String())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Store) UpdateNodeSettings(ctx context.Context, id, status string, overMem, overCPU float64, reservedMB int, labels map[string]string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE nodes SET status=$2, overcommit_memory=$3, overcommit_cpu=$4, reserved_mb=$5, labels=$6 WHERE id=$1`,
		id, status, overMem, overCPU, reservedMB, labels)
	return err
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM nodes WHERE id=$1`, id)
	return err
}

// Instances.

type Instance struct {
	ID               string    `json:"id"`
	ServiceID        string    `json:"service_id"`
	DeploymentID     *string   `json:"deployment_id"`
	NodeID           string    `json:"node_id"`
	ContainerID      string    `json:"container_id"`
	IP               string    `json:"ip"`
	Port             int       `json:"port"`
	Status           string    `json:"status"`
	MemoryRequestMB  int       `json:"memory_request_mb"`
	CPURequestMillis int       `json:"cpu_request_millis"`
	MemoryUsageMB    int       `json:"memory_usage_mb"`
	MemoryPeakMB     int       `json:"memory_peak_mb"`
	CPUUsageMillis   int       `json:"cpu_usage_millis"`
	Restarts         int       `json:"restarts"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

const instanceCols = `id, service_id, deployment_id, node_id, container_id, ip, port, status, memory_request_mb, cpu_request_millis, memory_usage_mb, memory_peak_mb, cpu_usage_millis, restarts, created_at, updated_at`

func scanInstance(row pgx.Row) (*Instance, error) {
	var i Instance
	err := row.Scan(&i.ID, &i.ServiceID, &i.DeploymentID, &i.NodeID, &i.ContainerID, &i.IP, &i.Port, &i.Status, &i.MemoryRequestMB,
		&i.CPURequestMillis, &i.MemoryUsageMB, &i.MemoryPeakMB, &i.CPUUsageMillis, &i.Restarts, &i.CreatedAt, &i.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &i, nil
}

func collectInstances(rows pgx.Rows, err error) ([]Instance, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Instance, error) {
		i, err := scanInstance(r)
		if err != nil {
			return Instance{}, err
		}
		return *i, nil
	})
}

func (s *Store) CreateInstance(ctx context.Context, i *Instance) (*Instance, error) {
	return insertInstance(ctx, s.q(ctx), i)
}

func insertInstance(ctx context.Context, q Querier, i *Instance) (*Instance, error) {
	return scanInstance(q.QueryRow(ctx, `INSERT INTO instances (service_id, deployment_id, node_id, container_id, port, status, memory_request_mb, cpu_request_millis)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+instanceCols,
		i.ServiceID, i.DeploymentID, i.NodeID, i.ContainerID, i.Port, i.Status, i.MemoryRequestMB, i.CPURequestMillis))
}

func (s *Store) InstanceByID(ctx context.Context, id string) (*Instance, error) {
	return scanInstance(s.q(ctx).QueryRow(ctx, `SELECT `+instanceCols+` FROM instances WHERE id=$1`, id))
}

func (s *Store) SetInstanceRunning(ctx context.Context, id, ip string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE instances SET status='running', ip=$2, updated_at=now() WHERE id=$1`, id, ip)
	return err
}

func (s *Store) SetInstanceStatus(ctx context.Context, id, status string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE instances SET status=$2, updated_at=now() WHERE id=$1`, id, status)
	return err
}

func (s *Store) DeleteInstance(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM instances WHERE id=$1`, id)
	return err
}

func (s *Store) InstancesForService(ctx context.Context, serviceID string) ([]Instance, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+instanceCols+` FROM instances WHERE service_id=$1 ORDER BY created_at`, serviceID)
	return collectInstances(rows, err)
}

func (s *Store) InstancesForNode(ctx context.Context, nodeID string) ([]Instance, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+instanceCols+` FROM instances WHERE node_id=$1`, nodeID)
	return collectInstances(rows, err)
}

func (s *Store) AllInstances(ctx context.Context) ([]Instance, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+instanceCols+` FROM instances ORDER BY created_at`)
	return collectInstances(rows, err)
}

// ApplyContainerStatus records live stats reported by an agent heartbeat. A 'lost' instance
// (its node went offline) comes back as running|failed when the node reports its container again.
func (s *Store) ApplyContainerStatus(ctx context.Context, nodeID string, c proto.ContainerStatus) error {
	status := "running"
	if !c.Running {
		status = "failed"
	}
	_, err := s.q(ctx).Exec(ctx, `UPDATE instances SET memory_usage_mb=$3, memory_peak_mb=GREATEST(memory_peak_mb, $3), cpu_usage_millis=$4,
		restarts=$5, ip=CASE WHEN $6<>'' THEN $6 ELSE ip END,
		status = CASE WHEN status IN ('running','failed','lost') THEN $7 ELSE status END, updated_at=now()
		WHERE node_id=$1 AND container_id=$2`, nodeID, c.ContainerID, c.MemoryMB, c.CPUMillis, c.Restarts, c.IP, status)
	return err
}

// NodeCommitted sums resource requests of live instances per node.
func (s *Store) NodeCommitted(ctx context.Context) (map[string][2]int, error) {
	return nodeCommitted(ctx, s.q(ctx))
}

func nodeCommitted(ctx context.Context, q Querier) (map[string][2]int, error) {
	rows, err := q.Query(ctx, `SELECT node_id, COALESCE(sum(memory_request_mb),0), COALESCE(sum(cpu_request_millis),0)
		FROM instances WHERE status IN ('starting','running') GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int{}
	for rows.Next() {
		var id string
		var mem, cpu int
		if err := rows.Scan(&id, &mem, &cpu); err != nil {
			return nil, err
		}
		out[id] = [2]int{mem, cpu}
	}
	return out, rows.Err()
}

// PlaceInstance picks a node and records the instance under one advisory lock, so concurrent
// placements see each other's reservations instead of all fitting into the same free space.
// pick gets the nodes and committed resources as read inside the lock.
func (s *Store) PlaceInstance(ctx context.Context, inst *Instance, pick func(nodes []Node, committed map[string][2]int) (*Node, error)) (*Instance, *Node, error) {
	tx, err := s.q(ctx).Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('placement'))`); err != nil {
		return nil, nil, err
	}
	nodes, err := queryNodes(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	committed, err := nodeCommitted(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	node, err := pick(nodes, committed)
	if err != nil {
		return nil, nil, err
	}
	inst.NodeID = node.ID
	out, err := insertInstance(ctx, tx, inst)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return out, node, nil
}
