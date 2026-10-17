# Phase 2 — log: copytruncate và Follow có giới hạn

File cần sửa:
- `internal/runtime/runtime.go`: thay `rotateLog`, sửa một dòng trong `startTask`.
- `internal/runtime/metrics.go`: `Follow`, thêm `capLog`/`CapLogs` cạnh `Logs`.
- `internal/agent/agent.go`: thêm một dòng trong `case <-tick.C`.

## Thay đổi

1. **Hằng số**, đặt trong `metrics.go`: `maxLogBytes = 10 << 20`, `maxLineBytes = 64 << 10`.

2. **`capLog(path string)`**, thay cho `rotateLog`. Xoá hẳn `rotateLog`.
   - `Stat`. Lỗi, hoặc `size < maxLogBytes`, thì return.
   - Mở file, `Seek(size-maxLogBytes)`, `io.Copy` sang `path+".1.tmp"`, `Rename` thành `path+".1"`.
   - `os.Truncate(path, 0)`.
   - Comment: shim mở log bằng O_APPEND, nên truncate khi nó vẫn đang ghi là an toàn; byte ghi giữa lúc copy và lúc truncate sẽ mất.
   - Lỗi ở bất kỳ bước nào: `slog.Warn` rồi return. Đừng truncate nếu copy chưa xong.

3. **`(r *Runtime) CapLogs()`**: chụp danh sách id dưới `r.mu`, rồi gọi `capLog(r.LogPath(id))` ngoài lock.

4. **`startTask`**: `rotateLog(logPath)` → `capLog(logPath)`.

5. **Agent**: trong `case <-tick.C:` thêm `a.rt.CapLogs()`. Không thêm ticker mới.

6. **`Follow`**:
   - `reader := bufio.NewReaderSize(f, maxLineBytes)`.
   - Trạng thái: `partial []byte` (độ dài ≤ `maxLineBytes`), `skip bool`.
   - Vòng lặp: `chunk, err := reader.ReadSlice('\n')`.
     - `err == nil` (gặp `\n`): nếu `skip` thì `skip = false`, reset `partial`, `continue`. Ngược lại emit `partial+chunk` (trim `\n`, cắt ở `maxLineBytes`), reset `partial`.
     - `err == bufio.ErrBufferFull`, hoặc `len(partial)+len(chunk) > maxLineBytes`: nếu `!skip` thì emit `(partial+chunk)[:maxLineBytes] + "…[truncated]"` rồi đặt `skip = true`. Reset `partial`, `continue`.
     - EOF: nếu `!skip` thì `partial = append(partial, chunk...)`. Phải copy, vì slice của `ReadSlice` mất hiệu lực sau lần đọc kế tiếp. Sau đó chờ 300ms hoặc ctx giống hiện tại.
   - Nhánh seek-0 khi `st.Size() < pos`: thêm `partial = partial[:0]` và `skip = false`.
   - Có thể tách phần cắt dòng thành một hàm thuần để test, nếu điều đó làm Follow đơn giản hơn. Không bắt buộc.

## Xác minh

- `go build ./... && go vet ./internal/...`.
- Kiểm tra bằng grep: `rotateLog` không còn xuất hiện.
- Test nằm ở phase 3.

## KHÔNG làm

- Không giữ nhiều hơn một bản `.1`, không nén.
- Không giới hạn dòng trong `tailFile`.
- Không đổi định dạng IO của task (`cio.LogFile` giữ nguyên).
- Không thêm config hay flag.
