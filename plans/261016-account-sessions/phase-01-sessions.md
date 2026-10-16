# Phase 1 — Thu hồi credential + access_token chỉ cho SSE

File: `internal/store/users.go`, `internal/server/api.go`, `internal/server/api_auth.go`, `internal/server/api_users.go`, `docs/API.md`, test `internal/server/sessions_test.go`.

## Thay đổi

1. **Store:** `RevokeCredentials(ctx, userID, keepSessionHash string) error` chạy trong một transaction:
   - `DELETE FROM sessions WHERE user_id=$1 AND token_hash <> $2`;
   - `DELETE FROM api_tokens WHERE user_id=$1`.
2. **`authed`:**
   - Khi xác thực bằng session thì đưa `util.HashToken(tok)` vào context (key mới cạnh `userKey`). Thêm helper `currentSessionHash(r) string`; khi xác thực bằng API token thì trả rỗng.
   - Chỉ đọc `access_token` từ query khi `r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs/stream")`, và cập nhật comment.
3. **`handleChangePassword`:** sau `UpdatePassword` gọi `RevokeCredentials(u.ID, currentSessionHash(r))`. Nếu lỗi thì trả 500.
4. **`handle2FADisable`:** sau `SetTOTP(..., false)` cũng làm như bước 3.
5. **`handleUpdateUser`:**
   - Dời validate mật khẩu (độ dài) lên **trước** `UpdateUser`, để không còn cập nhật dở dang.
   - Nếu đổi hoặc reset mật khẩu, hay tắt 2FA, thì `RevokeCredentials(u.ID, "")` (keep rỗng). Gọi một lần ở cuối, không gọi hai lần.
   - Trường hợp admin tự sửa chính mình: admin tự reset mật khẩu của mình qua đây thì cũng bị đăng xuất. Chấp nhận; ghi một dòng comment.
6. **docs/API.md:8:** "chỉ cho các route `…/logs/stream`".

## Test (handler, qua mux thật)

- `TestChangePasswordRevokesOthers`: user có session A, session B và API token T. Đổi mật khẩu bằng A → A gọi `/me` được 200; B và T nhận 401.
- `TestAdminPasswordResetRevokesAll`: admin reset user X (`reset_password:true`) → mọi session và token của X nhận 401. Session của admin vẫn 200.
- `TestDisable2FARevokes`: tự tắt (keep session hiện tại) và admin tắt (`disable_2fa:true`, xoá hết). Cần đặt secret và tạo mã TOTP hợp lệ trong test; phải tương thích với Phase 2 nếu Phase 2 làm trước.
- `TestQueryTokenOnlyOnStreams`: `GET /api/v1/me?access_token=<session>` → 401. `GET /api/v1/services/<uuid ngẫu nhiên>/logs/stream?access_token=<session>` → **khác 401** (404 hay 403 đều được, miễn là đã qua bước xác thực).

## KHÔNG làm

- Xoá session hiện tại rồi cấp session mới.
- Thu hồi khi bật 2FA, hay khi đổi is_admin.
- Chặn API token `paas_` trong query (chỉ chặn theo route).
