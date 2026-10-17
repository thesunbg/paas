# Phase 1 — try-lock cho reconcile và sleep

File cần sửa:
- `internal/store/store.go`
- `internal/server/server.go`
- `internal/server/reconcile.go`

## Thay đổi

1. **`store.TryAdvisoryLock(ctx, key) (func(), bool)`**, đặt cạnh `AdvisoryLock`.
   - Lấy một connection, chạy `SELECT pg_try_advisory_lock(hashtext($1))` đúng một lần.
   - Lấy được lock: trả về hàm release giống hệt `AdvisoryLock`, kèm `true`.
   - Không lấy được, hoặc `Acquire`/query lỗi: `conn.Release()` rồi trả `nil, false`. Không retry.
   - Comment ngắn: dùng cho các vòng lặp nền, vì chúng phải bỏ qua service đang bận thay vì xếp hàng chờ.

2. **`(s *Server) tryLock(serviceID) (func(), bool)`**, đặt trong `server.go` cạnh `lock`.
   - Lấy mutex từ map giống `lock`. Nên tách phần lấy mutex thành helper `serviceMutex(id)` để hai hàm dùng chung.
   - `m.TryLock()` trả false thì return false.
   - Sau đó gọi `s.store.TryAdvisoryLock(ctx, "service:"+id)`. Fail thì `m.Unlock()` rồi return false.
   - Trả về hàm release theo đúng thứ tự như `lock`: nhả advisory trước, mutex sau.

3. **Chia `reconcileService`:**
   - `reconcileService(ctx, id)`: gọi `tryLock`, bận thì return. Comment: "busy services are skipped; the next reconcile tick converges them". Toàn bộ caller hiện có giữ nguyên: loop, `scaleService`, `startService`, `api_services.go:405`.
   - `reconcileLocked(ctx, id)`: thân hàm hiện tại, caller phải giữ lock sẵn.
   - `wakeService`: thay dòng `s.reconcileService(ctx, svc.ID)` bằng `unlock := s.lock(svc.ID); s.reconcileLocked(ctx, svc.ID); unlock()`. Comment: wake là request đang được đợi, nên chờ lock thay vì bỏ qua; build không giữ lock nên thời gian chờ ngắn.

4. **`reconcileOnce(ctx)`**: chuyển nguyên thân vòng `for` của `reconcileLoop`, phần sau `select`, thành một hàm. `continue` khi `AllServices` lỗi đổi thành `return`. `reconcileLoop` chỉ còn ticker và lời gọi `s.reconcileOnce(ctx)`.

5. **`sleepIdle`**:
   - Trước `go`, gọi `unlock, ok := s.tryLock(svc.ID)`. Không lấy được thì `continue`.
   - Đổi `putToSleep(ctx, id)` thành `putToSleep(ctx, id, unlock)`: `defer unlock()` ở đầu và bỏ `s.lock` bên trong.
   - Mutex và advisory lock đều cho phép nhả từ goroutine khác. Advisory lock gắn với connection chứ không gắn với goroutine.

## Xác minh

- `go build ./... && go vet ./internal/...`.
- Kiểm tra bằng grep: `s.lock(` không còn xuất hiện trong `reconcileService` hay `putToSleep`.

## KHÔNG làm

- Không đổi chữ ký `lock` hay `AdvisoryLock`.
- Không thêm tham số `blocking bool`, không tạo interface Locker. Chỉ hai hàm cụ thể.
- Không đụng vào `sem` 4 hay ticker 10s.
