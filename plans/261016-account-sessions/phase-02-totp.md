# Phase 2 — 2FA: giới hạn thử + chống dùng lại mã

File: `internal/store/migrations/011_totp_last_step.sql` (mới), `internal/store/users.go`, `internal/totp/totp.go` (+ `totp_test.go`), `internal/server/api_auth.go`, `internal/server/github_login.go`, test `internal/server/totp_test.go`.

## Thay đổi

1. **Migration:** `ALTER TABLE users ADD COLUMN totp_last_step BIGINT NOT NULL DEFAULT 0;`. Đây là thay đổi schema duy nhất của plan. Không cần thêm cột này vào `userCols`.
2. **totp:** thay `Verify` bằng `Match(secret, code string, now time.Time) (step int64, ok bool)`. `step` là `t.Unix()/30` của cửa sổ khớp, và giữ nguyên ±1 bước cũng như so sánh constant-time. Không giữ lại `Verify`: hết người gọi thì xoá.
3. **Store:** `ConsumeTOTPStep(ctx, userID string, step int64) (bool, error)` chạy `UPDATE users SET totp_last_step=$2 WHERE id=$1 AND totp_last_step < $2` và trả về true khi `RowsAffected()==1`. Vì là một câu lệnh nguyên tử, nó an toàn với nhiều instance.
4. **Server:** helper `s.verifyTOTP(ctx, userID, secret, code string) bool` gọi Match rồi Consume; lỗi DB thì trả false. Thay cả 4 call-site sang helper này: `handleLogin`, github_login.go:207, `handle2FAEnable` và `handle2FADisable`.
5. **Giới hạn thử ở enable và disable:**
   - Key `"2fa:"+u.ID` (cùng key với github_login) trong `loginGuard`.
   - Nếu `!allowed` → 429. Mọi lần sai đều gọi `fail`: mã sai, mật khẩu sai ở disable, và mã bị dùng lại.
   - Nếu Phase 3 đã đổi chữ ký guard (thêm `max`) thì dùng ngưỡng 10.

## Test

- `totp`: `Match` trả về đúng step cho các cửa sổ -1, 0 và +1; sai mã thì `ok=false`.
- `TestTOTPReplayRejected`: user có 2FA. Login với mã M → 200. Login lại ngay với cùng M → 401 `invalid two-factor code`.
- `TestTOTPOlderStepRejected`: dùng mã của bước t, rồi mã của bước t-1 (vẫn trong cửa sổ) → bị từ chối.
- `Test2FAEnableRateLimited`: 10 mã sai vào `/me/2fa/enable` → lần thứ 11, dù mã đúng, vẫn 429.
- `Test2FADisableRateLimited`: tương tự, với mật khẩu sai.

## KHÔNG làm

- Lưu replay in-memory.
- Reset `totp_last_step` khi setup secret mới: step tăng theo thời gian, nên giá trị cũ vô hại.
- Recovery codes.
