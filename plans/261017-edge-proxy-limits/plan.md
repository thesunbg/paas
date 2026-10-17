---
title: Giới hạn tài nguyên của edge proxy (HTTP idle/header, TCP cap + thu hồi kết nối)
status: implemented
created: 2026-10-17
brainstorm: ../reports/brainstorm-261017-edge-proxy-limits.md
---

# Giới hạn tài nguyên của edge proxy

Mục tiêu:
- Server HTTP của edge và control plane đóng kết nối keep-alive rảnh sau 120s, và từ chối header lớn hơn 64 KB.
- Mỗi TCP route giữ tối đa 512 kết nối.
- `apply()` đóng các kết nối TCP đang mở khi route bị gỡ, khi IP rời allowlist, hoặc khi upstream mà kết nối đang dùng không còn trong `Upstreams` (trường hợp failover).
- Re-apply với route không đổi thì không đóng gì.

## Phase

| # | File | Nội dung | Phụ thuộc |
|---|---|---|---|
| 1 | [phase-01-http.md](phase-01-http.md) | helper `edgeServer`, `IdleTimeout` và `MaxHeaderBytes` cho 4 server, test 431 | — |
| 2 | [phase-02-tcp.md](phase-02-tcp.md) | `tcpConn`/`conns`/`closed`, admit/attach/`stale`/`kill`, apply quét kết nối, test | — |

Hai phase độc lập (khác file, trừ chung package) nên chạy song song được.

## Trạng thái

Đã implement cả 2 phase. `go vet ./...`, `gofmt`, `go test ./...` và `go test -race ./internal/proxy/` xanh. Chưa thử trên edge thật.

## Tiêu chí xong (toàn plan)

- `go test -race ./internal/proxy/` xanh, bao gồm các test mới ở phase 1 và phase 2.
- `go vet ./...` và `go test ./...` xanh.

## Quyết định đã chốt

- Không có idle deadline cho TCP: dựa vào keepalive 15s mặc định của Go để dọn peer chết.
- Cap theo route là hằng 512, khai báo dạng `var` để test hạ xuống. Không có cap toàn cục, không có config.
- `MaxHeaderBytes` = 64 KB, `IdleTimeout` = 120s. Không đặt `ReadTimeout` hay `WriteTimeout`.
- Không sửa `agent.go:505` (static origin chỉ nghe trên mesh).
