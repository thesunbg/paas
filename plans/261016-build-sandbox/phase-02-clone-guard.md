# Phase 2 — Clone guard

File: package mới nhỏ cho hàm kiểm IP (vd `internal/netguard/netguard.go`), `internal/server/notify.go`, `internal/build/build.go`, test.

## Thay đổi

1. `netguard.Public(ip net.IP) bool`, chuyển nguyên logic từ notify.go:37-38 (loopback, private, link-local, unspecified, multicast, CGNAT 100.64/10). notify.go gọi lại hàm này, hành vi không đổi. Hai người dùng: notify và build.
2. `checkout()`:
   - Bỏ `http://`. Chỉ còn `https://`, hoặc `file://`/`/` khi `AllowLocalGit`.
   - `url.Parse`. `u.User != nil` → từ chối (không nhận credential nhúng trong URL). Lấy `host`, `port` (mặc định 443).
   - `net.DefaultResolver.LookupIPAddr(ctx, host)`. Rỗng, hoặc **bất kỳ** IP nào `!Public` → lỗi `repository host %s resolves to a non-public address`. Chọn IP đầu tiên để ghim.
   - Thêm vào args của **mọi** lệnh git trong checkout (clone và fetch): `-c http.curloptResolve=<host>:<port>:<ip>` (IPv6 viết trong `[]`), `-c http.followRedirects=false`, `-c protocol.allow=never`, `-c protocol.https.allow=always`. Với local: `protocol.file.allow=always` và không resolve.
   - Có một cờ test/trusted giống `AllowPrivateNotifyTargets` thì dùng lại `AllowLocalGit` hoặc thêm một field `AllowPrivateGit` trên Builder. Chỉ thêm nếu test cần; thử `httptest` TLS trên 127.0.0.1 trước, bị chặn thì mới thêm.
3. Lệnh git nhận `-c` trước subcommand: `git -c k=v clone ...`. `runStreaming` hiện truyền `args...` thẳng nên chỉ cần ghép thêm phía trước.

## Test

- `http://github.com/a/b` → lỗi, git không chạy.
- `https://127.0.0.1/a/b`, `https://localhost/a/b`, `https://[::1]/x`, `https://user:pw@github.com/a/b` → lỗi trước clone.
- Unit test thuần cho hàm dựng args (host/port/ip → đúng các `-c`). Không cần mạng.
- `internal/server` notify test cũ vẫn xanh.

## KHÔNG làm

- Fallback khi git <2.37 (xem Phase 0).
- Allowlist host.
