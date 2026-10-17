# Brainstorm: D1 — lock theo service

## Vấn đề

- `runDeployment` (`internal/server/deploy.go:109`) giữ `s.lock(serviceID)` suốt cả build. Build gọi `requestAgent` với timeout 50 phút.
- Khi redeploy một service đang `running`, status vẫn là `running`, nên `reconcileLoop` vẫn reconcile nó và `reconcileService` bị chặn ở `s.lock`.
- `reconcileLoop` dùng `sem` 4 slot và đợi `wg.Wait()`. Chỉ một build là cả tick đứng chờ, bốn build thì cả bốn slot bị chiếm. Trong thời gian đó:
  - `MarkStaleNodesOffline` không chạy, node chết không bị phát hiện, workload không được reschedule.
  - Không service nào khác được reconcile.
  - `sleepIdle` không chạy, vì nằm sau `wg.Wait()`.
- `AdvisoryLock` chiếm một connection của pool (max 32) trong suốt thời gian giữ lock. 8 deploy chạy song song nghĩa là 8 connection bị giữ tới 50 phút.
- `sleepIdle` tạo `go putToSleep` cho mỗi service idle ở mọi tick. Nếu lock đang bị giữ, các goroutine này xếp hàng chồng lên nhau.

## Sự thật đã xác minh

- `ClaimQueuedDeployment` có `NOT EXISTS (... status IN ('building','deploying'))`, tức DB đã đảm bảo mỗi service chỉ có một deploy tại một thời điểm. Lock lúc build không bảo vệ thêm gì.
- Sau build có `svc, _ = s.store.ServiceByID(...)` bỏ qua lỗi. Hiện tại `deleteService` phải chờ lock nên không xoá được giữa chừng. Bỏ lock lúc build thì service có thể bị xoá trong lúc build, dẫn tới **panic nil làm sập control plane**. Phải sửa cùng lúc.
- `reconcileService` có các caller sau: `reconcileLoop`, `scaleService`, `startService`, `api_services.go:405`, `wakeService`.

## Quyết định (đã chốt với user)

| # | Quyết định | Lý do |
|---|---|---|
| 1 | Pha build không giữ lock. Chỉ lấy `s.lock` sau khi build xong, cho tới hết rollout. | DB đã serialize deploy-vs-deploy. Pha build không đụng vào instance. |
| 2 | Dưới lock, đọc lại service. Service đã bị xoá thì return. Service đã `stopped` thì đánh dấu deployment `cancelled` với lý do "service stopped during build". | Sửa lỗi panic nil. Tôn trọng ý định mới nhất của người dùng (vừa stop service). |
| 3 | `reconcileService` dùng try-lock. Đang bận thì bỏ qua, tick 10s sau sẽ bù. | Vòng lặp không bao giờ bị chặn bởi lock nữa. |
| 4 | `sleepIdle` gọi try-lock **trước** `go`. Lấy được lock mới tạo goroutine, và goroutine đó tự nhả lock. | Không sinh goroutine khi service đang bận. |
| 5 | **Wake vẫn dùng lock chặn (lệch khỏi brief).** | Wake là request người dùng đang đợi. Try-lock trùng với reconcileLoop sẽ trả "could not be started" giả. Khi build không còn giữ lock, thời gian chờ chỉ còn bằng thời gian rollout. |
| 6 | Thêm `store.TryAdvisoryLock`: một lần `pg_try_advisory_lock`; không lấy được thì trả connection về pool ngay. | Đi cặp với `sync.Mutex.TryLock` (Go 1.26 đã có). |
| 7 | Tách thân `reconcileLoop` ra thành `reconcileOnce(ctx)`. | Test gọi được một tick mà không phải đợi ticker 10s. |

### Hình dạng code dự kiến

- `server.go`: thêm `tryLock(id) (func(), bool)` cạnh `lock`. Cùng dùng map mutex: `m.TryLock()`, sau đó `store.TryAdvisoryLock`, advisory fail thì `m.Unlock()`.
- `reconcileService` (try-lock) gọi `reconcileLocked(ctx, svc)`. `wakeService` dùng `s.lock` rồi gọi `reconcileLocked`. Vì đã có hai caller thật với hai kiểu lock khác nhau, việc tách hàm là có lý do.
- `scaleService`, `startService`, `api_services:405` giữ `reconcileService` (try-lock). Bị bỏ qua thì tick sau bù, chậm tối đa khoảng 10s. Chấp nhận được.
- `putToSleep(ctx, id)` đổi thành `sleepLocked(ctx, id, unlock)`, hoặc nhận lock từ caller.

## Phương án đã loại

- **Bỏ hẳn lock trong reconcile, dựa vào status.** Rollout và reconcile cùng đụng vào instance. Không có lock thì scale sai số replica. Loại.
- **Wake dùng try-lock rồi poll upstream.** Thêm vòng chờ và timeout chỉ để thay một lock chặn mà giờ đã ngắn. Loại theo YAGNI.
- **Tách `MarkStaleNodesOffline` ra ticker riêng.** Sau khi có try-lock, vòng lặp không còn bị chặn bởi lock. Vòng lặp vẫn có thể chậm vì agent request chậm, nhưng đó là vấn đề khác. Chưa làm, ghi lại đây.
- **Chạy `reconcileLoop` thật trong test.** Ticker cứng 10s làm test chậm và dễ flaky. Loại.

## Rủi ro và nợ ghi nhận (không làm trong D1)

- `AdvisoryLock` lặp vô hạn khi `ctx` bị huỷ (`Acquire` lỗi thì sleep rồi lặp lại). Hiện mọi caller đều truyền `context.Background()` nên chưa nổ.
- `failDeployment` set service về `running` khi `CurrentDeploymentID != nil`. Nếu service bị `putToSleep` trong lúc build rồi build fail, service sẽ bị đánh thức lại. Tác hại nhỏ: tick sau nó ngủ lại.
- Service đang running với sleep bật vẫn có thể bị cho ngủ trong lúc đang build. Rollout sẽ bật lại. Chấp nhận được.

## Tiêu chí xong / kiểm chứng

Test chạy với Postgres thật, skip nếu không có `PAAS_TEST_DATABASE_URL`:
1. Khởi động NATS nhúng (`bus.StartServer` + `ConnectInProcess`) cùng một fake agent cho node `build`. Agent subscribe `CmdBuild` và chặn lại cho tới khi test giải phóng.
2. Tạo 4 service git đang `running` có current deployment. Mỗi service có một instance `running` trên node B, node B có heartbeat quá 20s.
3. Queue 4 deployment, chạy `runDeployment` trong goroutine, đợi cả 4 tới được fake agent.
4. Gọi `reconcileOnce` với deadline 5s. Assert:
   - hàm trả về trước deadline;
   - node B ở trạng thái `offline`;
   - instance trên B bị xoá (reconcile đã chạy trên chính các service đang build).
5. Test phụ: giữ `s.lock(id)` bằng tay, `reconcileOnce` vẫn trả về ngay, và instance của service đó không bị đụng tới.
6. Test phụ: xoá service trong lúc build, giải phóng build. Không panic, deployment không chuyển sang `active`. Stop service trong lúc build thì deployment chuyển sang `cancelled`.
7. `go test ./...` và `go vet` đều xanh.

## Bước tiếp theo

Viết `plans/261017-service-lock/plan.md`. Ba phase:
1. `tryLock` + `TryAdvisoryLock` + `reconcileOnce`.
2. Thu hẹp lock trong `runDeployment` + xử lý service bị stop/xoá.
3. Test harness NATS + 3 test.
