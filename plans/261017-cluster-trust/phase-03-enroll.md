# Phase 3: enroll, join token, định danh theo subject (phía control plane)

## Việc
0. (Chuyển từ phase 2, bước 2–3) Thêm setting `cluster_ca_cert` và `cluster_ca_key` (key mã hoá bằng `secrets.Box`), load hoặc tạo bằng `pki.NewCA` theo kiểu insert-if-absent rồi đọc lại. Truyền `ca.ServerTLS()` vào `StartServer` và `ca.RouteTLS()` vào `ClusterOptions.TLS`. Xoá các comment TRANSITIONAL/TODO trong `bus.go` và `server.go`.
1. Migration `016_cluster_trust.sql`:
   - `ALTER TABLE nodes ADD COLUMN nkey TEXT UNIQUE;`
   - `CREATE TABLE join_tokens (hash BYTEA PRIMARY KEY, roles TEXT[] NOT NULL, expires_at TIMESTAMPTZ NOT NULL, used_at TIMESTAMPTZ, created_by UUID);`
2. Store:
   - `CreateJoinToken(hash, roles, ttl, by)`.
   - `ConsumeJoinToken(hash) (roles, error)`: `UPDATE ... SET used_at=now() WHERE hash=$1 AND used_at IS NULL AND expires_at>now() RETURNING roles`.
   - `EnrollNode(name, nkey, roles) (*Node, oldNkey string, err)`: upsert theo name, gán nkey và roles, cấp net_index theo đúng logic của `UpsertNode` (tách logic này ra để dùng chung, không copy).
   - `NodeByNkey(nkey)`.
   - Đổi `UpsertNode` thành `UpdateNodeRegistration(id, req)`: chỉ cập nhật node đã tồn tại, không bao giờ insert.
   - Consume token và upsert node phải nằm trong cùng một transaction. Nếu không, enroll fail giữa chừng sẽ đốt mất token.
3. Server:
   - Dựng `bus.Auth{Internal: random 32B, Lookup: store.NodeByNkey, Legacy: cluster_token nếu PAAS_NATS_LEGACY_TOKEN=1}`. Truyền TLS của phase 2 vào `StartServer`. Mọi chỗ gọi `ConnectInProcess` dùng `Internal`.
   - `POST /api/v1/cluster/join-tokens {roles}`, chỉ admin gọi được: trả 409 kèm thông điệp rõ ràng nếu `PublicURL` không bắt đầu bằng `https://`. Trả `{token, expires_at, command}`. **Xoá** `GET /api/v1/cluster/join` (`api_nodes.go:138`).
   - `POST /api/v1/cluster/enroll {token, name, nkey}`, không yêu cầu đăng nhập:
     - validate nkey bằng `nkeys.IsValidPublicUserKey`;
     - consume token;
     - gọi `EnrollNode`; nếu có `oldNkey` thì kick nó;
     - trả `{node_id, nats_url, ca_pem}`. `nats_url` suy ra giống `handleJoinInfo` cũ, đổi scheme thành `tls://`.
     - Rate limit: dùng lại limiter của login nếu có sẵn (`grep -rn limiter internal/server`), không có thì không làm.
   - Kick: `handleDeleteNode` publish `paas.cp.kick {nkey}`. Mọi control plane subscribe subject này (không dùng queue) rồi gọi `auth.Kick(s.ns, nkey)`. Node không có quyền với `paas.cp.*` vì không nằm trong `NodePermissions`.
   - Đổi subject trong `proto`: `SubjNodeRegister(id)` là `paas.node.<id>.register`, `SubjNodeHeartbeat(id)` là `paas.node.<id>.heartbeat`. Server dùng `HandleQueue` trên `paas.node.*.register` và `QueueSubscribe` trên `paas.node.*.heartbeat`, lấy id từ `m.Subject` (`bus.Handle` hiện chưa đưa subject cho handler; thêm biến thể nhận `*nats.Msg` hoặc viết inline). Bỏ `validToken` và field `Token` trong 2 handler mới.
   - Legacy: giữ handler cũ trên `paas.agent.register` và `paas.agent.heartbeat` **chỉ khi** cờ legacy bật, có comment điều kiện xoá. Agent ở nhánh legacy (cả binary cũ lẫn binary mới chưa có identity) vẫn đi qua các subject cũ.
4. Dashboard và API nodes: thêm field `legacy: nkey IS NULL` để admin biết node nào cần join lại.

## Tiêu chí xong
- Handler test (dùng Postgres thật, giống `testserver_test.go`):
  - join-tokens trả 409 khi PublicURL là http;
  - token dùng lần hai trả 401, token hết hạn trả 401;
  - enroll cùng name hai lần thì giữ nguyên net_index và thay nkey;
  - register trên `paas.node.X.register` cập nhật đúng node X.
- `go test ./internal/server/ ./internal/store/` xanh.

## KHÔNG làm
- Sửa agent, việc của phase 4.
- Firewall cổng 4222.
- Cache ở NodeByNkey.
