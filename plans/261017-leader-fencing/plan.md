---
title: Fencing cho leader (epoch), FailStuck reset service, cancel chỉ webhook, placement atomic
status: implemented
created: 2026-10-17
---

# D3 — Fencing cho leader

Lý do và các phương án đã loại: `plans/reports/brainstorm-261017-leader-fencing.md`.

## Lỗi

Conn giữ lock leader đứt thì Postgres nhả lock ngay và một control plane khác lên làm leader. Leader cũ vẫn tiếp tục chạy deploy (`WithoutCancel`) và các goroutine `context.Background()`, rồi ghi đè kết quả `FailStuckDeployments` của leader mới. Đi kèm ba lỗi khác:
- Service kẹt ở `deploying` sau FailStuck.
- Webhook huỷ lệnh rollback đang queued.
- Hai lần placement đồng thời overcommit một node.

## Phase 1 — Epoch và executor fenced (store)

Migration `013_leader_fence.sql`:
- `leader(id int PRIMARY KEY CHECK (id=1), epoch bigint NOT NULL)`, seed một row `(1, 0)`.
- Hàm `paas_fence(n bigint)` (plpgsql, VOLATILE):
  - `PERFORM pg_advisory_xact_lock_shared(<K_FENCE>)`.
  - Sau đó, **ở một statement riêng** (để có snapshot mới), `IF (SELECT epoch FROM leader WHERE id=1) <> n THEN RAISE EXCEPTION USING ERRCODE='P0001', MESSAGE='paas: fenced'`.

`internal/store/fence.go`:
- `WithEpoch(ctx, n) context.Context` và `epochFrom(ctx) (int64, bool)`.
- `ErrFenced`. Map lỗi `P0001` có message `paas: fenced` sang `ErrFenced`.
- `TakeLeadership(ctx, conn *pgx.Conn) (int64, error)`: chạy trên conn của leader. Mở tx, `pg_advisory_xact_lock(K_FENCE)` (exclusive), `UPDATE leader SET epoch=epoch+1 RETURNING epoch`, commit.
- `CheckFence(ctx) error`: ctx không có epoch thì trả nil, có thì `SELECT paas_fence($1)`.
- Interface `querier` gồm `Exec`, `Query`, `QueryRow`, `Begin`. Hàm `(s *Store) q(ctx) querier`: ctx không có epoch thì trả `s.DB`, có thì trả `fencedDB{pool, epoch}`.
  - `Exec` / `QueryRow`: gửi một `pgx.Batch` gồm `SELECT paas_fence($e)` và câu lệnh (implicit tx, 1 round trip). Fence fail thì câu lệnh không commit.
  - `Begin`: begin tx thật, rồi chạy `SELECT paas_fence` làm câu đầu tiên.
  - `Query` (nhiều row): fence nếu batch làm được gọn. Nếu không, cho đi thẳng xuống pool **và** grep xác nhận không có `s.DB.Query` nào là lệnh ghi. Ghi kết luận vào comment.
- Thay mọi `s.DB.X(ctx, ...)` trong `internal/store` (khoảng 133 chỗ) bằng `s.q(ctx).X(ctx, ...)`. Thêm `(s *Store) Q(ctx) querier` exported, và đổi 19 chỗ `s.store.DB.X` trong `internal/server` sang `s.store.Q(ctx)`.
- Giữ `Store.DB` (pool) cho `Close`, test và `AdvisoryLock`.

## Phase 2 — Leader có phạm vi (server)

- `leaderLoop`: lấy được lock thì gọi `epoch, err := s.store.TakeLeadership(ctx, conn)`. Lỗi thì đóng conn rồi lặp lại. Tiếp theo `lctx = store.WithEpoch(lctx, epoch)` và log epoch. Từ đây mọi lệnh đều dùng `lctx`.
- `deploy.go:67`: đổi `s.runDeployment(context.WithoutCancel(ctx), d)` thành `s.runDeployment(ctx, d)`.
- Audit cây gọi từ `leaderLoop` (deployWorker, resumeProvisioning, syncBaseDomain, cronLoop, metricsLoop, reconcileLoop, maintenanceLoop, dbMetricsLoop, haLoop). Mọi `context.Background()` / `WithoutCancel` **trong cây này** đổi thành ctx được truyền xuống. Đã biết các chỗ sau:
  - goroutine tạo standby trong `haTick`;
  - goroutine PITR trong `promoteStandby`;
  - `go s.cleanupStatic(context.Background(), ...)` cuối `runDeployment`;
  - `removeVolumesWhenOnline` (thêm tham số ctx).

  Hàm dùng chung với đường API (`stopService`, `deleteService`, `startInstance`, `promoteStandby`) chỉ dùng ctx được truyền vào, không tự tạo ctx mới.
- `requestAgent`: gọi `s.store.CheckFence(ctx)` trước `bus.Request`, lỗi thì return luôn.

## Phase 3 — Ba lỗi đi kèm (store và server)

- **FailStuckDeployments**: viết lại thành một câu lệnh CTE.
  - Các deployment `building`/`deploying`: deployment nào là `services.current_deployment_id` thì đặt `active`, còn lại đặt `failed` với error `interrupted by control plane restart`.
  - Các service có deployment vừa bị đánh và đang ở `deploying`: có current thì về `running`, không có thì về `failed`. Service `stopped`/`sleeping` thì không đụng.
  - `DELETE FROM instances WHERE status='starting' AND deployment_id IN (<các deployment bị failed>)`.
- **CancelOlderQueued**: SQL thêm `AND trigger='webhook'`. Trong `QueueDeployment` chỉ gọi khi `trigger == "webhook"`. Sửa comment cho đúng ngữ nghĩa.
- **PlaceInstance**: thêm `store.PlaceInstance(ctx, inst *Instance, pick func(nodes []Node, committed map[string][2]int) (*Node, error)) (*Instance, *Node, error)`.
  - Trong một tx (fenced nếu ctx có epoch): `pg_advisory_xact_lock(hashtext('placement'))`, rồi đọc nodes và committed (dùng chung SQL với `NodeCommitted`, chạy trong tx), rồi `pick`, rồi set `inst.NodeID`, rồi INSERT, rồi commit.
  - `startInstance` thay đoạn `Nodes` + `NodeCommitted` + `Pick` + `CreateInstance` bằng `PlaceInstance` với closure gọi `scheduler.Pick`.

## Phase 4 — Test (Postgres thật, skip khi không có `PAAS_TEST_DATABASE_URL`)

Theo mẫu `lock_test.go` / `spec_version_test.go`.
1. **Fence store**:
   - `TakeLeadership` trả n. Ghi với `WithEpoch(n)` thì OK. Bump lên n+1 (gọi `TakeLeadership` trên một conn khác), rồi ghi với `WithEpoch(n)` thì nhận `ErrFenced` và row không đổi. Ghi với ctx không có epoch thì OK.
   - Thứ tự: một tx fenced đang mở thì `TakeLeadership` phải chờ tới khi tx đó commit.
2. **Leader cũ không ghi được gì**: dùng NATS nhúng và fake build agent chặn ở `CmdBuild`, như D1.
   - `runDeployment` chạy với `WithEpoch(n)`. Khi tới được agent thì bump epoch, chạy `FailStuckDeployments`, rồi thả agent ra.
   - Assert: deployment vẫn `failed`, service ở status mà FailStuck đặt, không có row instance mới, và fake run-agent không nhận được `CmdRun`.
3. **Rollback không bị webhook huỷ**: `redeploy(..., "rollback")`, rồi `QueueDeployment(..., "webhook")`. Rollback vẫn `queued`. Hai webhook liên tiếp thì cái đầu thành `cancelled`.
4. **Placement**: một node 1000 MB, 10 goroutine gọi `PlaceInstance` 200 MB thì đúng 5 cái thành công, 5 cái nhận `ErrNoCapacity`.
5. **FailStuck**:
   - Deployment `deploying` + service `deploying` không có current: deployment `failed`, service `failed`.
   - Có current khác: service `running`.
   - Deployment chính là current: deployment `active`.
   - Service `stopped` thì giữ nguyên.
   - Instance `starting` của deployment bị failed thì bị xoá. Instance `running` của deployment khác thì giữ.

Cuối cùng: `go build ./... && go vet ./... && go test ./...`.

## Giao việc

- P1 core (`fence.go` + migration) → `implementer`. Đổi call-site `s.DB.` → `s.q(ctx).` → `executor`, **sau khi** P1 core đã chốt interface.
- P2 → `implementer` (phải audit cây gọi).
- P3 → `implementer`. Chạy song song với P1 được, chỉ đụng các hàm riêng.
- P4 → `implementer` sau cùng, sau đó `verifier`.

## KHÔNG làm

- Fencing phía agent (epoch trong proto, broadcast).
- Đổi `WithoutCancel` / `Background` trong handler API và heartbeat. Chúng không thuộc cây leader.
- `FOR SHARE` trên row `leader`. Check-then-act. Guard thủ công từng UPDATE.
- Row lock theo node có retry.
- Tính job run vào committed.
- Chạy lại deployment bị gián đoạn. Pause autodeploy sau rollback.
- `lock_timeout` cho bump. Tách fence riêng cho metricsLoop (chờ có số đo).
- Sửa availability khi conn half-open (`tcp_keepalives_*`).

## Rủi ro

- Phase 1 đổi khoảng 150 call-site. Diff lớn nhưng đều cùng một khuôn. Review bằng `git diff --stat` cộng grep `s\.DB\.` chỉ còn ở `Close`, test và advisory lock.
- `Query` không fence được: cần kết luận rõ là không có lệnh ghi nào đi qua `Query`.
- Check trước `requestAgent` vẫn hở vài ms. Nợ có chủ ý. Điều kiện để làm fencing ở agent: có sự cố thật, hoặc có thêm lệnh agent không idempotent. Ghi điều kiện này trong comment tại `requestAgent`.

## Tiêu chí xong

- Test 1–5 ở Phase 4 pass với Postgres thật.
- `grep -rn WithoutCancel internal/server/deploy.go` không còn kết quả.
- Không còn `context.Background()` nào trong cây gọi từ `leaderLoop`; test hoặc review xác nhận.
