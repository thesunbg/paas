# Phase 2 — Vòng đời sync + nft bắt buộc

File: `internal/network/network.go`, `internal/agent/agent.go`, `internal/runtime/runtime.go`.

## Thay đổi

1. `Manager.fwMu sync.Mutex`: `SyncFirewall` khoá nó quanh `Ruleset()` + `nft -f`. Không dùng `m.mu`, vì `Ruleset` tự khoá `m.mu`.
2. Xoá `SyncFirewall` khỏi `Attach` (network.go ~203) và khỏi cuối `Recover` (runtime.go ~759). Sửa comment của `Restore` (bỏ câu "refreshes the firewall").
3. `agent.Run`: lời gọi `SyncFirewall` hiện có (agent.go ~113, sau register, trước `Recover`) đổi thành **trả error** thay vì `slog.Warn`. Như vậy agent không chạy khi thiếu nft hoặc lần apply đầu thất bại. Lời gọi này phải nằm trước `a.rt.Recover(ctx)` (hiện đã đúng).
4. `Configure` và `SetShared`: khi giá trị thực sự đổi, gọi lại `SyncFirewall` **sau khi nhả `m.mu`**. Lỗi thì `slog.Error`, giữ bảng cũ. Kiểm tra ai đang gọi `SetShared` (`grep`): nếu không có caller nào thì xoá `SetShared` thay vì thêm logic.
   - Lưu ý: `Configure` được gọi trong `register` trước lời gọi sync ở bước 3. Lần sync trong Configure khi đó là thừa nhưng vô hại. Có thể truyền cờ hoặc chỉ sync khi đã sync ít nhất một lần, tuỳ cách nào gọn hơn. Không thêm state chỉ để tránh một lần sync thừa nếu nó làm code phức tạp.
5. `SyncFirewall` giữ thông báo "nft not found". Lời gọi ở agent.go sẽ bọc nó lại thành error khi khởi động.

## KHÔNG làm

- Không sync theo chu kỳ (ticker). Kernel giữ bảng, ruleset tĩnh nên không có gì để drift. Nếu có tác nhân ngoài flush ruleset (ví dụ firewalld reload) thì để xử lý sau, khi gặp thật.
- Không đụng NAT, iptables.

## Xong khi

- `go build ./... && go test ./...` xanh.
- `grep -n SyncFirewall` chỉ còn lời gọi ở agent.go và trong `Configure`/`SetShared`.
- Thủ công: đổi `PATH` để không có nft, agent thoát với error rõ ràng.

---

## Kết quả (2026-10-16) — XONG

- Xoá sync trong `Attach` và cuối `Recover`. `agent.Run` trả error khi sync thất bại, kể cả thiếu nft. Lời gọi này chạy trước `Recover`.
- Xoá `SetShared` vì không có caller nào.
- **Không thêm sync vào `Configure`**: nó chỉ được gọi một lần trong `register`, ngay trước lời gọi sync ở `Run`.
- **Không thêm `fwMu`**: `SyncFirewall` giờ chỉ có một caller chạy một lần, nên không có race để chặn. Khi nào có caller thứ hai chạy đồng thời thì mới thêm.
- `go build`, `go vet`, `go test ./internal/network/... ./internal/agent/...` xanh, gofmt sạch.
