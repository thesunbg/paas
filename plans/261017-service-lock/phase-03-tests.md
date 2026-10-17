# Phase 3 — Test với Postgres thật

Các file:
- `internal/server/testserver_test.go`: thêm helper.
- `internal/server/lock_test.go`: file mới.

Test skip như các test hiện có khi không có `PAAS_TEST_DATABASE_URL`.

## Harness

Thêm `newTestServerBus(t) *Server`. Hàm này gọi `newTestServer(t)` rồi bổ sung những phần mà deploy và reconcile cần:

- `s.locks`, `s.evictedOn`, `s.wake`, `s.traffic`, `s.scale`: khởi tạo giống `New` trong `server.go`.
- `s.ClusterToken`: một token ngẫu nhiên.
- `s.ns = bus.StartServer("127.0.0.1:-1", tok, t.TempDir())`. Port `-1` nghĩa là NATS tự chọn port ngẫu nhiên.
- `s.nc = bus.ConnectInProcess(...)`.
- `s.hub = NewLogHub(st, s.nc)`.
- `t.Cleanup` đóng `nc` và gọi `ns.Shutdown()`.

Nếu `publishAll`, `notify` hay hàm nào khác cần thêm field: chỉ thêm đúng field đó, ghi lý do vào báo cáo.

Thêm `fakeBuildAgent(t, s, nodeID) (started <-chan string, release func())`:
- Subscribe `proto.AgentSubject(nodeID, proto.CmdBuild)`.
- Mỗi khi nhận request: gửi `DeploymentID` vào `started`, rồi chặn cho tới khi `release` được gọi.
- Sau đó reply `proto.BuildReply{Image: "img:test"}`.
- Không cần subscribe `CmdRun`. Cả 3 test đều không cần rollout thành công.

Seed bằng store API có sẵn: `UpsertNode`, `CreateService`, `CreateDeployment`, `SetServiceCurrentDeployment`, `CreateInstance`. Chỗ nào không có API thì dùng SQL trực tiếp:
- Một node `build`: online, có role `build`.
- Một node `B`: online, `last_heartbeat = now() - 1 minute`.
- Mỗi service: kind `git`, status `running`, có current deployment cũ (dùng image, không cần build), và một instance `running` trên node B.

## Test

1. **`TestReconcileRunsDuringBuilds`**: đây là tiêu chí xong của D1.
   - Seed 4 service như trên. Với mỗi service, tạo deployment `queued`, rồi `ClaimQueuedDeployment` và `go s.runDeployment(ctx, d)`.
   - Đọc `started` đủ 4 lần, timeout 10s. Lúc này cả 4 build đang bị chặn.
   - Gọi `s.reconcileOnce(ctx)` với `context.WithTimeout` 5s, chạy trong goroutine, rồi đợi kết quả bằng `select` có timeout. Hết 5s mà chưa xong thì `t.Fatal("reconcile blocked by builds")`.
   - Assert:
     - node B có status `offline`;
     - 4 instance trên B không còn, hoặc ít nhất không còn `running`. Cụ thể: `reconcileLocked` xoá instance `lost` khi node không online, và điều này chứng minh reconcile đã chạy trên chính các service đang build.
   - Cleanup: gọi `release()` và đợi 4 goroutine kết thúc, dùng WaitGroup bọc quanh `runDeployment`. Mục đích là không để rò goroutine sang lúc `DROP DATABASE`.

2. **`TestReconcileSkipsLockedService`**:
   - Seed 1 service, gọi `unlock := s.lock(id)` bằng tay.
   - `reconcileOnce` phải trả về trong 5s.
   - Instance của service đó vẫn còn nguyên, vì service bị bỏ qua.
   - Gọi `unlock()`, rồi `reconcileOnce` lần nữa: instance bị xử lý.

3. **`TestDeployServiceChangedDuringBuild`**: chia hai subtest.
   - `stopped`: build đang bị chặn thì `SetServiceStatus(stopped)`, rồi `release()`, đợi `runDeployment` xong. Deployment phải ở `cancelled`, service vẫn `stopped`.
   - `deleted`: build đang bị chặn thì xoá service bằng `DeleteService` của store hoặc SQL, rồi `release()`. `runDeployment` phải trả về mà không panic. Bọc bằng `defer recover` trong goroutine của test; nếu bắt được panic thì gọi `t.Error`.

## Xác minh

- `PAAS_TEST_DATABASE_URL=... go test ./internal/server/ -run 'TestReconcile|TestDeploy' -race -count=3` xanh.
- Thử ngược trên một nhánh tạm: revert phase 2. Test 1 phải fail vì timeout, chứng minh test thật sự bắt được lỗi.
- `go test ./...` không có DB vẫn xanh (các test này skip).

## KHÔNG làm

- Không chạy `reconcileLoop` hay `deployWorker` thật, vì ticker sẽ làm test chậm.
- Không mock store. Chỉ dùng Postgres thật và NATS nhúng thật.
- Không test `sleepIdle` hay wake riêng. Thay đổi ở đó nhỏ và đi cùng đường `tryLock` đã có test 2 phủ.
- Không thêm thư viện test mới.
