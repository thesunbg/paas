# Phase 1 — vòng đời container: netns, Run, orphan, Once

File cần sửa:
- `internal/runtime/runtime.go`

## Thay đổi

1. **`attachNet(ctx, id string, m *managed) (*netns.NetNS, error)`**, tách từ `Run:236-248`.
   - `netns.NewNetNS(filepath.Join(r.dataDir, "netns"))`.
   - `_ = r.net.Detach(ctx, m.Req.NetIndex, id, nsh.GetPath())`. Bước này giải phóng reservation IPAM cũ của cùng id (lần thử trước, hoặc trước khi reboot). Giữ nguyên comment hiện có.
   - `r.net.Attach(...)`. Lỗi thì `nsh.Remove()` rồi return lỗi.
   - Trả `nsh` cùng IP. Caller tự gán `m.NetNS` và `m.IP` dưới `r.mu` nếu `m` đã nằm trong map. Trong `Run`, `m` chưa vào map nên gán thẳng.

2. **`Run`: dọn sạch khi lỗi.**
   - Dùng `attachNet`.
   - Ngay sau khi attach thành công, đặt `ok := false` và một `defer`: nếu `!ok` thì `r.net.Detach(...)`, `nsh.Remove()`, `os.RemoveAll(r.containerDir(id))`.
   - Đặt `ok = true` ngay trước `r.saveState`/đưa vào map. Từ đó trở đi `r.Remove` là chỗ dọn duy nhất, như hiện tại.
   - Bỏ các dòng Detach/Remove chép tay trong nhánh lỗi của `createContainer`.

3. **`ensureNetNS(ctx, ctr client.Container, id string) error`**, gọi trong `startTask` sau `LoadContainer` và trước `NewTask`.
   - Lấy `m := r.containers[id]` dưới lock. Nếu `nil` thì return nil.
   - `closed, err := netns.LoadNetNS(m.NetNS).Closed()`. Có lỗi thì return lỗi. `!closed` thì return nil.
   - `slog.Info("netns gone, re-attaching", "id", id)`.
   - `nsh, ip, err := r.attachNet(...)`.
   - `spec, err := ctr.Spec(ctx)`. Tìm `spec.Linux.Namespaces` có `Type == specs.NetworkNamespace`, gán `Path = nsh.GetPath()`.
   - `ctr.Update(ctx, client.UpdateContainerOpts(client.WithSpec(spec)))`. Lỗi thì Detach + `nsh.Remove()` rồi return lỗi.
   - Dưới lock: `m.NetNS, m.IP = nsh.GetPath(), ip`, rồi `r.saveState(id, m)`.
   - Comment: "a reboot unmounts every netns; the path baked into the spec must follow the new one".

4. **`Recover`, nhánh orphan** (không có `state.json`):
   - `if t, err := ctr.Task(cctx, nil); err == nil { _, _ = t.Delete(cctx, client.WithProcessKill) }`.
   - Trước khi xoá container, đọc `ctr.Spec(cctx)` để lấy path của namespace network. Path khác rỗng thì `netns.LoadNetNS(p).Remove()`.
   - `ctr.Delete(cctx, client.WithSnapshotCleanup)`. Có lỗi thì log bằng `slog.Warn`, không im lặng.
   - Comment: "Remove releases the address before the state dir goes, so an orphan holds no IP".

5. **`Recover`, job Once**: sau khi unmarshal và đưa `m` vào map, nếu `m.Req.Once` thì `slog.Info("dropping job left from before restart", "id", id)`, gọi `_ = r.Remove(ctx, id, time.Second)` rồi `continue`. Không gọi `net.Restore`, không `Wait`, không `startTask`.

## Xác minh

- `go build ./... && go vet ./internal/...`.
- Kiểm tra bằng grep: `netns.NewNetNS` chỉ còn xuất hiện trong `attachNet`.
- Phần integration nằm ở phase 3.

## KHÔNG làm

- Không đổi thứ tự trong `Remove`. Không giữ state khi `ctr.Delete` lỗi, vì như vậy `Recover` sẽ resurrect container (xem brainstorm).
- Không thêm label `paas.netindex`, không quét thư mục.
- Không gọi `ensureNetNS` ngoài `startTask`.
- Không thay đổi `managed` (không thêm field).
