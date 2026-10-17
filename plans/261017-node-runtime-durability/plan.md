---
title: Độ bền của runtime trên node
status: implemented
created: 2026-10-17
brainstorm: ../reports/brainstorm-261017-node-runtime-durability.md
---

# Độ bền của runtime trên node

Mục tiêu:
- Reboot host thì container chạy lại được. Cơ chế: netns chết được tạo lại, CNI được attach lại, spec được cập nhật, và tất cả xảy ra trong `startTask`.
- Container ghi 1 GB không có `\n` không làm agent phình RAM (Follow có giới hạn), và log trên đĩa bị chặn ở 10 MB + 1 bản `.1`.
- Không leak IP/netns khi `Run` lỗi giữa chừng. Nhánh orphan dọn được cả task đang sống.
- `Recover` không bao giờ chạy lại job Once.

## Phase

| # | File | Nội dung | Phụ thuộc |
|---|---|---|---|
| 1 | [phase-01-lifecycle.md](phase-01-lifecycle.md) | `attachNet`, `ensureNetNS` trong `startTask`, dọn khi `Run` lỗi, orphan, Once trong `Recover` | — |
| 2 | [phase-02-logs.md](phase-02-logs.md) | `capLog` (copytruncate), `CapLogs` trong tick của agent, Follow giới hạn 64 KB | — |
| 3 | [phase-03-tests.md](phase-03-tests.md) | Unit test cho Follow và capLog; integration test cho reboot, unfence, Once, leak | 1, 2 |

Phase 1 và 2 cùng sửa `runtime.go`, nhưng ở các hàm khác nhau: phase 2 chỉ thay `rotateLog` và một dòng gọi trong `startTask`. Có thể làm song song nếu merge cẩn thận. Phase 3 làm cuối.

## Trạng thái

Đã implement cả 3 phase. Unit test, `go vet` (kể cả `-tags integration`) và `go test ./...` xanh.
Integration test chạy trên node 202.92.6.129 (2026-10-17, binary build chéo, sau đó đã xoá `pn7` và ns `paas-test`): `TestRunHTTPContainer`, `TestRecoverAfterReboot`, `TestUnfenceAfterReboot`, `TestRecoverDropsJob`, `TestRunLeakOnNetFiles` đều PASS. Reboot host thật chưa được thử.

## Tiêu chí xong (toàn plan)

- `go test ./internal/runtime/ -run 'TestFollow|TestCapLog' -race` xanh, không cần root.
- `sudo go test -tags integration ./internal/runtime/ -run 'TestRecover|TestRunLeak'` xanh: giả lập reboot → container healthy; job Once bị xoá; Run lỗi không để lại netns hay reservation IPAM.
- `go vet ./...` và `go test ./...` xanh.

## Quyết định đã chốt

- Job Once trong `Recover`: kill + Remove, không bao giờ start lại.
- Log: copytruncate 10 MB, copy 10 MB đuôi sang `.1`. Chạy trong tick 5s sẵn có và trong `startTask`.
- Follow: cắt ở 64 KB + `…[truncated]`, bỏ phần còn lại tới `\n`.
- Orphan: chỉ làm vế (a). Vế (b) "giữ state khi `ctr.Delete` lỗi" **đã bị loại sau khi chốt**, vì nó làm `Recover` khởi động lại container đang bị xoá. Xem brainstorm.
- IP được phép đổi sau reboot. Heartbeat cập nhật DB, proxy và mesh đọc IP từ DB.

## KHÔNG làm

- Giữ IP cũ qua CNI arg `IP=`.
- Bỏ netns file để attach CNI vào `/proc/<pid>/ns/net`.
- Quét `containers/*` để tìm thư mục không có container trong containerd.
- Logger `binary://` hay để agent tự giữ pipe stdout.
- Giới hạn độ dài dòng trong `tailFile`/`Logs` (đã bị chặn 1 MB, không phải nguồn phình RAM).
- Thêm config cho 10 MB, 64 KB, hay số bản `.N`. Dùng hằng số.
- Sửa lỗi crash giữa Stop và `ctr.Delete` của `Remove` (có từ trước F).
