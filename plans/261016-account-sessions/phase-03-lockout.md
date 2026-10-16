# Phase 3 — Lockout email+IP

File: `internal/server/api_auth.go`, `internal/server/github_login.go` (cập nhật call-site), test `internal/server/lockout_test.go`.

## Thay đổi

1. `attemptGuard.allowed(key string, max int) bool`. `fail(key)` giữ nguyên, vì `recent` chỉ tỉa theo `failureWindow`. Xoá hằng `maxFailures` và thay bằng ba hằng có tên:
   - `maxPerEmailIP = 10`;
   - `maxPerIP = 50`;
   - `maxPerEmail = 100`;
   - cộng `maxPer2FA = 10` cho key `2fa:`.
2. `handleLogin` dùng ba key: `email+"|"+ip`, `ip` và `"email:"+email`. Tiền tố là để key email không trùng với key IP. Bị chặn khi bất kỳ key nào hết lượt; khi `failed()` thì tăng cả ba.
3. github_login.go: giữ `"2fa:"+userID` và `ip`, chỉ cập nhật ngưỡng theo chữ ký mới.
4. Comment trên `attemptGuard`:
   - lý do của ba ngưỡng (email+IP bảo vệ từng tài khoản; IP chặn rải mật khẩu nhưng chịu được NAT; email là trần cho tấn công phân tán);
   - "in-memory per control plane; remove when counters move to a table".

## Test

- `TestLockoutPerEmailIP`: 10 lần sai cho victim từ IP1 → IP1 nhận 429; IP2 với đúng mật khẩu → 200.
- `TestLockoutPerIP`: 50 lần sai từ IP1, rải trên 50 email khác nhau → lần kế tiếp từ IP1 nhận 429, kể cả với email mới.
- `TestLockoutPerEmailCap`: 100 lần sai cho victim từ 100 IP khác nhau → IP mới nhận 429.

Đặt IP qua `RemoteAddr`. Với loopback thì `clientAddr` đọc XFF, nên dùng IP không phải loopback cho đơn giản.

## KHÔNG làm

- Lưu counter vào DB.
- Backoff tăng dần, hay CAPTCHA.
