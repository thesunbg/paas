# Brainstorm: giới hạn tài nguyên của edge proxy (G)

Ngày: 2026-10-17. Plan: [../261017-edge-proxy-limits/plan.md](../261017-edge-proxy-limits/plan.md)

## Vấn đề

**HTTP.** Có 3 server ở `internal/proxy/proxy.go:150,151,164` và 1 ở `internal/server/server.go:195`. Cả bốn chỉ đặt `ReadHeaderTimeout`.
- `IdleTimeout` = 0 thì Go lấy `ReadTimeout`, mà `ReadTimeout` cũng = 0. Hệ quả là kết nối keep-alive rảnh (cả h1 lẫn h2) sống mãi và giữ fd cùng goroutine.
- `MaxHeaderBytes` đang là mặc định 1 MB. Mỗi kết nối độc hại có thể bắt server giữ tới 1 MB header trong 15s.

**TCP** (`internal/proxy/tcp.go`):
- Không giới hạn số kết nối trên mỗi route.
- `apply()` đổi allowlist thì chỉ có kết nối **mới** bị ảnh hưởng, kết nối đang mở vẫn chạy tiếp. Gỡ route thì listener đóng, nhưng các pipe đang chạy vẫn sống. Thu hồi quyền mà không thu hồi được kết nối là lỗ hổng bảo mật.
- Failover của DB HA đổi `Upstreams`, nhưng kết nối cũ vẫn bám vào primary cũ đã bị fence. Client trong pool ăn lỗi ghi cho tới khi pool tự recycle.

## Sự thật đã kiểm tra

- `net.Listen` và `net.DialTimeout` của Go **mặc định bật TCP keepalive 15s** (Go ≥1.13; Go 1.23 dùng Idle 15s, Interval 15s, Count 9). Peer chết bị phát hiện trong khoảng 2–3 phút, ở cả hai phía của pipe.
- Upstream của TCP route là DB server trực tiếp (`dbaas.go:46`, `ServerServiceID`), không qua pgbouncer. `max_connections` lớn nhất trong các preset là 300 (xlarge).
- Systemd unit đặt `LimitNOFILE=1048576` (`installer/install.sh:285`). Edge sẽ hết RAM trước khi hết fd.
- `publishRoutes` chạy mỗi 30s, nên `apply()` được gọi lại liên tục với route không đổi. Điều này bắt buộc: re-apply mà không có thay đổi thì **không được** đóng kết nối nào.

## Phương án đã cân nhắc

| Chủ đề | Phương án | Kết luận |
|---|---|---|
| Idle TCP | Bỏ, dựa vào keepalive | **Chọn.** Keepalive đã dọn peer chết. Kết nối còn sống mà rảnh là pool hoặc `LISTEN` hợp lệ. Code thêm: 0 |
| | Idle 1h, xét cả hai chiều | Loại. Khoảng 25 dòng (timestamp dùng chung, gia hạn khi deadline hết), vẫn giết `LISTEN` rảnh, và gần như không thu thêm được gì so với keepalive |
| | Idle 5–10 phút như LB trên cloud | Loại. Làm đứt pool của app (lỗi `connection reset`). LB trên cloud tính cả keepalive probe là có hoạt động, còn deadline trong Go thì không thấy probe |
| Cap | Theo route, hằng 512 | **Chọn.** Lớn hơn mọi `max_connections` (≤300) cộng thêm phần dư cho kết nối đang handshake. Cô lập được giữa các tenant |
| | Thêm cap toàn cục | Loại. NOFILE là 1M, và số DB của một tenant đã bị giới hạn ở tầng khác. Chưa nêu được hỏng hóc cụ thể nào |
| | Chặn `Accept` khi đầy | Loại. Backlog của kernel đầy dần và client treo không rõ lý do. Đóng ngay thì rõ ràng hơn |
| Đóng conn khi `apply` | Route bị gỡ, IP mất allow, upstream mất | **Chọn.** Điều kiện upstream chỉ thêm 1 so sánh trên cùng cấu trúc, và sửa được việc client kẹt trên primary cũ sau failover |
| | Chỉ route bị gỡ và IP mất allow | Loại. Bỏ sót trường hợp failover |
| MaxHeaderBytes | 64 KB | **Chọn.** Rộng hơn mặc định của nginx (4×8 KB), an toàn với cookie/JWT lớn |
| | 32 KB / giữ 1 MB | Loại: 32 KB có rủi ro 431 với app tenant. Giữ 1 MB thì không thu nhỏ được trường hợp xấu nhất |

## Giải pháp

### HTTP
- `IdleTimeout: 120s` và `MaxHeaderBytes: 64 KB` cho cả 4 server. Ba server edge được gom về một helper `edgeServer(addr, h)` trong `proxy.go`, nên số dòng giảm.
- **Không** đặt `ReadTimeout` hay `WriteTimeout`, vì chúng giết SSE, websocket, upload lớn và download dài. `ReadHeaderTimeout` đã chặn slowloris.
- **Không** sửa `agent.go:505` (static origin): server này chỉ nghe trên mesh IP nội bộ, không ai ngoài cluster chạm được.

### TCP: một cấu trúc giải quyết cả cap lẫn việc đóng kết nối
```go
var maxConnsPerRoute = 512 // var để test hạ xuống; không phải config

type tcpConn struct {
    ip       netip.Addr
    client   net.Conn
    upstream string   // "" cho tới khi dial xong
    server   net.Conn
    dead     bool
}
type tcpListener struct {
    ln     net.Listener
    route  atomic.Pointer[tcpRoute]
    mu     sync.Mutex
    conns  map[*tcpConn]struct{}
    closed bool
}
func stale(r *tcpRoute, c *tcpConn) bool // !allowed(r, c.ip) || (c.upstream != "" && c.upstream ∉ r.Upstreams)
func (c *tcpConn) kill()                  // dead=true; đóng client; đóng server nếu có
```
- `serve` truyền `l` (thay vì snapshot route) vào `handle`.
- **admit**: dưới `l.mu`, làm `r := l.route.Load()`, rồi từ chối nếu `closed`, đã đủ cap, hoặc `!allowed(r, ip)`. Nếu nhận thì thêm vào `conns`, và `defer` gỡ ra khi xong. Kiểm tra allowlist nằm trong lock, và cùng `r` đó được dùng cho handshake.
- **attach** sau khi dial: dưới `l.mu`, nếu `dead` hoặc `stale(l.route.Load(), c)` thì đóng server và return. Nếu không thì gán `server` và `upstream`. Dial nằm ngoài lock.
- **apply**:
  - Route còn: `Store(tr)` rồi lock và `kill` mọi conn mà `stale(tr, c)`.
  - Route bị gỡ: lock, đặt `closed = true`, `kill` tất cả, rồi đóng `ln`.

**Vì sao không có race.** `apply` làm `Store` trước rồi mới Lock và quét. `admit`/`attach` thì Lock rồi mới `Load`. Mỗi conn hoặc bị lượt quét nhìn thấy, hoặc tự `Load` được route mới. Cửa sổ giữa lúc dial và lúc attach được đóng lại nhờ re-check `stale` khi attach.

- Chuẩn hoá `allowed(r, netip.Addr)`: IP được parse một lần lúc admit.
- Phải đóng cả hai đầu (client và server). Nếu chỉ đóng client, chiều server→client sẽ chặn trong `server.Read` cho tới khi DB tự đóng.

## Rủi ro

- Thu hồi allowlist hoặc failover sẽ cắt kết nối đang chạy, kể cả transaction dở. Đây là hành vi mong muốn, nhưng phải ghi rõ trong ghi chú UI/API của allowlist (chỉ một dòng doc).
- Cap 512 là hằng số. Nếu sau này thêm preset có `max_connections` > 512, phải nâng hằng số này. Đặt comment tại chỗ ghi rõ ràng buộc với `databases.Presets`.
- `kill()` gọi `Close` trong khi giữ `l.mu`. `Close` trên socket TCP không chặn lâu, nên chấp nhận được.

## Tiêu chí thành công

- Re-apply với route y hệt thì kết nối sống sót. Đây là test quan trọng nhất, vì `apply` chạy mỗi 30s.
- Bỏ IP khỏi allow, gỡ route, hoặc đổi upstream: client nhận EOF trong vòng 1s. Kết nối mới tới upstream mới chạy được.
- Cap = 2 (test hạ biến xuống): kết nối thứ 3 bị đóng ngay. Đóng một kết nối thì lại nhận được kết nối mới.
- HTTP: header 100 KB nhận 431, header 10 KB nhận 200.
- `go test -race ./internal/proxy/` và `go vet ./...` xanh.

## Không làm

- Idle deadline cho TCP; test ghim hành vi keepalive (đó là hành vi của stdlib).
- Cap toàn cục; cap theo IP nguồn; cap lấy từ `max_connections` của DB (sẽ phải đổi `model.TCPRoute`).
- `ReadTimeout` / `WriteTimeout` cho HTTP; sửa `agent.go:505`.
- Config option cho bất kỳ giá trị nào ở trên.
