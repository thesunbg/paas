# Phase 4: agent và proxy standalone dùng identity

## Việc
1. Identity nằm trong `<DataDir>/identity/`, thư mục 0700, mọi file 0600:
   - `node.json`: `{node_id, nats_url}`;
   - `seed.nk`: seed nkey;
   - `ca.pem`.
   Viết `loadIdentity` và `enroll` vào `internal/agent/identity.go`.
2. Lúc khởi động, khi `nc == nil` (agent standalone):
   - đã có identity: gọi `bus.ConnectNode(...)`;
   - chưa có identity nhưng có `PAAS_JOIN_URL` và `PAAS_JOIN_TOKEN`: sinh nkey, gọi `POST <url>/api/v1/cluster/enroll` (từ chối url không phải https), ghi identity xuống đĩa rồi mới connect. Fail thì trả error rõ ràng, systemd sẽ restart. Sau khi enroll thành công, xoá dòng `PAAS_JOIN_TOKEN` khỏi `/etc/paas/paasd.env`; xoá không được thì chỉ warn, vì token đã cháy rồi;
   - chưa có identity nhưng có `PAAS_CLUSTER_TOKEN`: nhánh legacy, giữ đúng hành vi cũ và log warn "node uses the legacy cluster token, re-join it". Comment ghi điều kiện xoá;
   - không có gì cả: báo lỗi.
3. Register và heartbeat chuyển sang `proto.SubjNodeRegister(id)` và `SubjNodeHeartbeat(id)` khi có identity. Bỏ `Token` trong payload. Ở nhánh legacy thì giữ nguyên subject cũ.
4. `config`: thêm `JoinURL` và `JoinToken`. `ClusterToken` của agent và proxy chỉ còn dùng cho legacy.
5. `cmd/paasd/main.go`:
   - `runAgent` không bắt buộc `PAAS_CLUSTER_TOKEN` nữa;
   - control plane gọi `runAgent` in-process bằng secret internal;
   - `runProxyWith` khi standalone đọc identity của node (cùng DataDir gốc `/var/lib/paas`, vì proxy chỉ có `DataDir/proxy`; cần một đường dẫn chung, ví dụ `PAAS_IDENTITY_DIR`, mặc định `/var/lib/paas/identity`). Chưa có identity thì chờ và retry: agent sẽ enroll sau một lúc.
6. Self-update: không đổi logic. Giờ chỉ control plane trả lời được `paas.agent.binary`, vì node không subscribe được subject này.

## Tiêu chí xong
- Unit test cho `enroll`: dùng httptest server, kiểm tra quyền file 0600, từ chối url http.
- `go test ./internal/agent/ ./cmd/...` xanh.
- Agent in-process trong control plane vẫn chạy bình thường (test server hiện có xanh).

## KHÔNG làm
- Tự enroll bằng cluster token.
- mTLS phía client.
- Đổi cơ chế self-update.
