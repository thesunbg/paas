# Phase 2: cap kết nối TCP và thu hồi kết nối khi apply

File: `internal/proxy/tcp.go`, `internal/proxy/tcp_test.go`.

## Cấu trúc dữ liệu

```go
// maxConnsPerRoute caps live connections per database port. Must stay above the
// largest max_connections in databases.Presets (300). var only so tests can lower it.
var maxConnsPerRoute = 512

type tcpConn struct {
    ip       netip.Addr
    client   net.Conn
    upstream string // "" until dialed
    server   net.Conn
    dead     bool
}

type tcpListener struct {
    ln     net.Listener
    route  atomic.Pointer[tcpRoute]
    mu     sync.Mutex // guards conns, closed and every tcpConn's fields
    conns  map[*tcpConn]struct{}
    closed bool
}
```

## Hàm

- `allowed(r *tcpRoute, ip netip.Addr) bool`. IP được parse một lần lúc admit, nhớ `Unmap()`.
- `stale(r *tcpRoute, c *tcpConn) bool`: trả `true` khi `r == nil`, hoặc `!allowed(r, c.ip)`, hoặc (`c.upstream != ""` và `c.upstream` không nằm trong `r.route.Upstreams`).
- `(c *tcpConn) kill()`: đặt `dead = true`, đóng `client`, và đóng `server` nếu khác nil. Chỉ được gọi khi đang giữ `l.mu`.
- `serve`: thay `go m.handle(c, l.route.Load())` bằng `go m.handle(c, l)`.
- `handle(client, l)`:
  1. Parse IP từ `client.RemoteAddr()`. Lỗi thì đóng và return.
  2. **admit**, dưới `l.mu`: `r := l.route.Load()`. Từ chối (đóng client và return) nếu `l.closed`, hoặc `len(l.conns) >= maxConnsPerRoute`, hoặc `r == nil`, hoặc không có upstream, hoặc `!allowed(r, ip)`. Nếu nhận thì thêm vào `conns`. `defer` gỡ khỏi `conns` dưới lock.
  3. Handshake giữ nguyên logic hiện có, dùng `r` lấy được ở bước admit.
  4. Dial nằm ngoài lock, giữ nguyên vòng lặp round-robin.
  5. **attach**, dưới `l.mu`: nếu `c.dead`, hoặc sau khi tạm gán `c.upstream = addr` mà `stale(l.route.Load(), c)` → đóng server và return. Nếu không thì gán `c.server = server`.
  6. `pipe(conn, server)` như cũ.
- `apply`:
  - Listener còn trong route mới: `l.route.Store(tr)`, sau đó lock, `kill` mọi `c` có `stale(tr, c)`, rồi unlock.
  - Listener bị gỡ: lock, đặt `l.closed = true`, `kill` tất cả, unlock, rồi `ln.Close()` và `delete` như cũ.
  - Listener mới: khởi tạo `conns`.

**Vì sao không có race** (ghi vào comment trong code):
- `apply` làm `Store` trước rồi mới Lock. `admit`/`attach` thì Lock rồi mới `Load`. Vì vậy mỗi conn hoặc bị lượt quét nhìn thấy, hoặc tự `Load` được route mới.
- Cửa sổ giữa lúc dial và lúc attach được đóng lại nhờ re-check `stale` khi attach.

## Test (`tcp_test.go`, tái dùng `echo`, `freePort`, `roundTrip`)

Dùng mode `plain` cho đơn giản. Mỗi test mở kết nối, `roundTrip` thành công, gọi `apply(...)`, rồi đọc với deadline 1s. Kỳ vọng là EOF hoặc lỗi; riêng test re-apply y hệt thì kỳ vọng `roundTrip` vẫn chạy.

1. `TestTCPReapplyKeepsConn`: `apply` lại đúng route đó, kết nối vẫn sống. **Bắt buộc**, vì `apply` chạy mỗi 30s.
2. `TestTCPAllowRevokeClosesConn`: bỏ `127.0.0.1/32` khỏi `Allow`, kết nối bị đóng.
3. `TestTCPRouteRemovedClosesConn`: `apply(nil)`, kết nối bị đóng.
4. `TestTCPUpstreamChangeClosesConn`: đổi `Upstreams` từ echo A sang echo B, kết nối cũ bị đóng, kết nối mới roundTrip được qua B.
5. `TestTCPConnCap`: đặt `maxConnsPerRoute = 2` (khôi phục bằng `t.Cleanup`). Mở 2 kết nối đều roundTrip được. Kết nối thứ 3 bị đóng ngay. Đóng một trong hai kết nối đầu, chờ ngắn, rồi kết nối mới phải roundTrip được.

## Tiêu chí xong
- `go test -race ./internal/proxy/ -run TestTCP -count=3` xanh.
- Các test TCP hiện có (postgres SSLRequest, direct TLS, mode tls) vẫn xanh.

## KHÔNG làm
- Idle/read deadline sau handshake. Deadline handshake 30s hiện có thì giữ nguyên.
- Cap toàn cục, cap theo IP nguồn, cap lấy từ `max_connections` của DB, hay thêm field vào `model.TCPRoute`.
- Đổi semantics của `pipe`, `bufConn`, hay logic handshake postgres/tls.
- Đóng kết nối khi chỉ `Mode` thay đổi (chưa có tình huống thật nào cần).
- Config option.
