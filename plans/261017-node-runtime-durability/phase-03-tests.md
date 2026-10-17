# Phase 3 — test

File cần tạo hoặc sửa:
- `internal/runtime/logs_test.go` (mới, không có build tag)
- `internal/runtime/runtime_integration_test.go` (`//go:build integration`, cần root + containerd + CNI)

## Unit (không cần root)

1. **`TestFollowLongLine`**
   - `r := &Runtime{dataDir: t.TempDir()}`, tạo `containers/<id>/output.log`.
   - Chạy `Follow` trong goroutine, đẩy các dòng nhận được vào channel.
   - Ghi 200 MB không có `\n` (theo từng khối 1 MB), sau đó ghi `"ok\n"`.
   - Assert: nhận đúng 2 dòng. Dòng 1 dài ≤ `maxLineBytes + len("…[truncated]")` và kết thúc bằng marker. Dòng 2 là `"ok"`.
   - Đo RAM: `runtime.GC()` + `ReadMemStats` trước và sau. `HeapInuse` không được tăng quá 8 MB.

2. **`TestFollowPartialAcrossPolls`**: ghi `"ab"`, chờ hơn 300ms, ghi `"c\n"`. Phải nhận đúng `"abc"`.

3. **`TestFollowAfterTruncate`**: ghi `"a\n"`, `capLog` (hoặc `os.Truncate`), ghi `"b\n"`. Phải nhận `"a"` rồi `"b"`.

4. **`TestCapLog`**
   - Mở fd `O_APPEND|O_WRONLY` (giả lập shim), ghi 15 MB có pattern nhận diện được.
   - `capLog`. Assert: `.1` dài đúng 10 MB và bằng 10 MB cuối; file gốc dài 0.
   - Ghi tiếp `"x\n"` qua fd cũ. Assert file gốc dài đúng 2 byte, tức là không có sparse hole.
   - File dưới 10 MB: `capLog` không làm gì.

## Integration (root)

Dùng lại setup của `TestRunHTTPContainer`.

5. **`TestRecoverAfterReboot`**
   - `Run` một container HTTP (image test hiện có).
   - Giả lập reboot: load task, `Kill(SIGKILL)` + `Wait` + `Delete`; `netns.LoadNetNS(m.NetNS).Remove()`; `rt.Close()`.
   - `rt2 := New(...)` với cùng `dataDir` và cùng `ns`, rồi `rt2.Recover(ctx)`.
   - Assert: trong 30s, `isRunning(id)` đúng và HTTP GET tới IP trong `Statuses()` trả 2xx/3xx/4xx. `Statuses()[i].IP` khác rỗng. Path netns trong spec trùng với `m.NetNS` mới.

6. **`TestUnfenceAfterReboot`**: như (5) nhưng `Fence` trước khi giả lập reboot. Sau `Recover`, container không chạy. Sau `Unfence`, nó chạy và healthy.

7. **`TestRecoverDropsJob`**
   - `Run` với `Once: true` và `Command: ["sleep 60"]`. Gọi `Run` trực tiếp, không dùng `RunJob`, để giả lập caller đã chết.
   - `rt.Close()`, `rt2 := New(...)`, `rt2.Recover(ctx)`.
   - Assert: `LoadContainer` trả NotFound, `containerDir` không còn tồn tại.

8. **`TestRunLeakOnNetFiles`**
   - Làm cho `writeNetFiles` lỗi: tạo sẵn `containers/<id>/resolv.conf` dưới dạng **thư mục**.
   - `Run` phải trả lỗi. Assert: `dataDir/netns` rỗng; dưới `dataDir/cni` không còn file nào chứa id đó (reservation của host-local).

## Xác minh

- `go test ./internal/runtime/ -race` xanh.
- `sudo go test -tags integration ./internal/runtime/ -run 'TestRecover|TestUnfence|TestRunLeak' -v` xanh.
- `go vet ./...` và `go test ./...` xanh.

## KHÔNG làm

- Không reboot VM thật trong test.
- Không mock containerd hay CNI. Integration test chạy với hàng thật, unit test chỉ đụng tới file.
