# Brainstorm — Tài khoản và phiên đăng nhập (việc C)

Ngày: 2026-10-16

## Vấn đề

1. **Đổi mật khẩu, reset hay tắt 2FA đều không thu hồi gì.** Session (TTL 30 ngày) và API token `paas_` vẫn sống, nên kẻ đã chiếm được cookie hay token giữ quyền truy cập dù nạn nhân đã đổi mật khẩu. Các chỗ liên quan: `handleChangePassword` (api_auth.go:194), `handleUpdateUser` (api_users.go:114-138), `handle2FADisable` (api_auth.go:401).
2. **`?access_token=` được chấp nhận ở mọi route** (api.go:224). Token nằm trong URL sẽ rơi vào log của proxy, vào history và vào Referer. Trong cây code hiện chỉ có `EventSource` của web dùng tới, mà web lại xác thực bằng cookie. Người dùng ngoài thì theo docs/API.md:8.
3. **2FA:**
   - `handle2FAEnable` và `handle2FADisable` không giới hạn số lần thử. Với một session bị đánh cắp, kẻ tấn công brute-force được mã 6 số, và qua `/disable` thì đoán được cả mật khẩu.
   - `totp.Verify` không ghi lại mã đã dùng, nên mã bị nhìn trộm vẫn dùng lại được trong 90 giây.
   - Có **bốn** chỗ gọi Verify: login, github_login.go:207, enable và disable.
4. **Lockout chỉ theo email** (api_auth.go:114). Ai cũng khoá được tài khoản nạn nhân bằng 10 lần thử sai.
5. **Không giới hạn số team** (api_auth.go:265). Mỗi team lại có quota project, service và database riêng, nên tạo nhiều team là lách được toàn bộ team limits.
6. **`handleAddMember`** có hai lỗ:
   - Ai cũng tạo được team và làm admin của team đó, rồi tạo tài khoản cho email bất kỳ. Đây là lối đăng ký vòng, bỏ qua `allow_signup` và `signupGuard`.
   - Với email đã tồn tại, handler trả về cả user object (name, is_admin, totp_enabled, github_login, created_at).

## Phát hiện khi scout

- **Control plane chạy được nhiều instance** (`leaderLoop`, server.go:332). Các guard in-memory (`loginGuard`, `signupGuard`) đếm riêng theo từng process: có N instance thì kẻ tấn công có N× số lần thử. Với lockout thì chấp nhận được, nhưng với chống dùng lại TOTP thì không, vì mã dùng ở instance A vẫn qua được ở instance B.
- **Không có hạ tầng test chạm DB.** `Store` là `*pgxpool` cụ thể, và 3 file test hiện có đều là pure func. Tiêu chí "mỗi mục một test handler" vì vậy kéo theo việc dựng harness.
- github_login.go:196 đã có tiền lệ dùng key `"2fa:"+userID` trong `loginGuard`, nên dùng lại cho enable và disable.

## Các phương án đã cân nhắc

| Chủ đề | Chọn | Loại | Lý do loại |
|---|---|---|---|
| Test harness | **PG thật qua `PAAS_TEST_DATABASE_URL`**, mỗi lần chạy một DB tạm, `t.Skip` khi chưa set, CI thêm service postgres:16 | Interface hoá Store | Refactor lớn, abstraction chỉ có một impl thật |
| | | embedded-postgres | Thêm dependency tải binary, test chậm |
| Thu hồi khi tự đổi mật khẩu | **Xoá các session khác + mọi API token**, giữ session hiện tại | Chỉ session | Token đã lộ vẫn sống |
| | | Xoá hết rồi cấp session mới | Phức tạp hơn mà không thêm an toàn đáng kể |
| Suất 1 cột | **`users.totp_last_step`** | Cột cho lockout | Khoá email+IP không thuộc về user (email có thể không tồn tại), nên cần cả một bảng chứ không phải một cột |
| | | Không đổi schema | TOTP replay xuyên instance |
| Lockout | **email+IP 10, IP 50, email 100** | Chỉ email+IP và IP | Không có trần cho tấn công phân tán nhiều IP vào một email |
| AddMember khi signup tắt | **Chỉ admin hệ thống được tạo tài khoản mới**. Khi signup bật thì áp thêm `signupGuard` | Cấm tuyệt đối | Admin hệ thống mất lối tắt, mà không thêm an toàn gì |
| Max teams | **Đếm membership role=owner, mặc định 3**, admin được miễn, 0 = không giới hạn | — | — |
| access_token | **Chỉ GET và path kết thúc `/logs/stream`** | Bỏ hẳn | docs/API.md công bố tham số này cho client ngoài |

**Sửa ngưỡng IP trong lockout.** Lúc hỏi, phương án tôi đưa ra là "email+IP 10, IP 10". Bộ ba đó tự mâu thuẫn: số lần thử của cặp email+IP không bao giờ vượt số lần thử của IP đó, nên khoá email+IP không bao giờ là khoá chặn trước. Vì vậy ngưỡng IP được nâng lên 50. Khi đó:
- email+IP (10) bảo vệ từng tài khoản trước một nguồn;
- IP (50) chặn rải mật khẩu nhiều email từ một nguồn, và chịu được văn phòng dùng chung NAT;
- email (100) là trần cho tấn công phân tán.

## Giải pháp chốt

1. **Harness** `internal/server/testserver_test.go`:
   - `newTestServer(t)` tạo DB `paas_test_<rand>`, chạy `Migrate`, dựng `&Server{store, box, cfg}` và trả về `http.Handler` thật (mux + `authed`).
   - Reset `loginGuard` và `signupGuard` cho mỗi test.
   - Có helper tạo user, login và lấy token.
2. **Thu hồi:**
   - `store.RevokeCredentials(ctx, userID, keepSessionHash)` chạy trong một transaction: xoá sessions có `token_hash <> keep`, rồi xoá mọi api_tokens.
   - `authed` đưa hash của session đang dùng vào context. Nếu request đi bằng API token thì hash rỗng, và token đó cũng bị xoá.
   - Tự đổi mật khẩu và tự tắt 2FA: keep = session hiện tại. Admin đổi hoặc reset mật khẩu, hay tắt 2FA cho user: keep = "".
3. **access_token:** `authed` chỉ đọc query khi `r.Method == GET && strings.HasSuffix(r.URL.Path, "/logs/stream")`. Sửa docs/API.md:8.
4. **TOTP:**
   - Migration `011_totp_last_step.sql`: `ALTER TABLE users ADD COLUMN totp_last_step BIGINT NOT NULL DEFAULT 0`.
   - `totp.Verify` đổi thành `totp.Match(secret, code, now) (step int64, ok bool)`.
   - `store.ConsumeTOTPStep(ctx, userID, step) (bool, error)` chạy `UPDATE … WHERE id=$1 AND totp_last_step < $2` và trả về true khi rows=1. Cách này không race kể cả khi có nhiều instance.
   - Helper `s.verifyTOTP(ctx, userID, secret, code) bool` gộp Match với Consume, và cả 4 call-site đều đi qua nó.
5. **Giới hạn thử ở enable và disable:** dùng key `"2fa:"+userID` trong `loginGuard` (tiền lệ ở github_login). Lần thử sai nào cũng tính, kể cả sai mật khẩu ở disable.
6. **Lockout:**
   - `attemptGuard.allowed` và `fail` nhận thêm `max`. `recent` chỉ tỉa các lần thử cũ hơn cửa sổ thời gian nên không cần biết ngưỡng.
   - Có ba key: `email|ip` (10), `ip` (50) và `email` (100).
   - Login bằng mật khẩu và nhánh 2FA của login dùng chung các key này.
7. **Max teams:**
   - Thêm `MaxTeams` (`max_teams`, mặc định 3) vào `TeamLimits`.
   - Đếm bằng `SELECT count(*) FROM memberships WHERE user_id=$1 AND role='owner'`.
   - Kiểm trong `handleCreateTeam` khi `limited(r)` đúng; trả lỗi theo đúng cách các quotaError khác đang trả.
   - Thêm một ô trong System.svelte.
8. **AddMember:**
   - Email chưa có tài khoản: nếu `allow_signup != "true"` và caller không phải admin hệ thống thì trả 403. Nếu signup bật và caller không phải admin hệ thống thì áp `signupGuard` như `handleRegister`.
   - Response bỏ user object, đổi thành `{"email","role","temporary_password"?}`. Sửa `AddMemberResponse` và Settings.svelte:123-124 cho khớp.

## Rủi ro / lưu ý

- **Đổi mật khẩu sẽ giết API token**, nên CI hay script đang dùng token sẽ hỏng. Đây là hành vi đúng ý; cần release note và thêm một dòng cảnh báo trên form đổi mật khẩu (tuỳ chọn, không bắt buộc).
- **Lockout vẫn in-memory theo từng instance** (N× số lần thử). Bỏ được khi chuyển counter vào bảng riêng, và việc đó là đổi schema nên nằm ngoài phạm vi.
- **AddMember vẫn để lộ việc email có tồn tại hay không.** Email mới thì nhận 403 (khi signup tắt) hoặc kèm temporary_password; email cũ thì nhận 201. Muốn đóng hẳn phải có luồng invite (bảng mới). Việc bỏ user object chỉ ngăn lộ dữ liệu, không ngăn dò sự tồn tại.
- **AddMember vẫn kéo được user bất kỳ vào team mà không cần họ đồng ý.** Cũng cần luồng invite mới đóng được. Ghi nhận ở đây, không làm.
- **Quota team có race:** hai request đồng thời có thể vượt quota thêm 1, giống các quota project hiện có. Chấp nhận.
- **`TeamLimits` là JSON trong settings.** Setting cũ chưa có `max_teams` sẽ unmarshal đè lên default. Cần kiểm rằng default 3 vẫn giữ khi key vắng mặt: `json.Unmarshal` không đụng tới field vắng mặt, nên vẫn đúng.

## Ngoài phạm vi (phát hiện khi scout, không làm)

- `mail.ParseAddress` chấp nhận dạng `"Name <x@y>"`, và AddMember không lower/trim email. Lỗi validate email, không phải lỗi phiên đăng nhập.
- Bật 2FA không thu hồi các session có từ trước khi bật. Phạm vi việc C không nhắc tới.
- `handleUpdateUser` ghi name/is_admin trước khi validate độ dài mật khẩu, nên có thể cập nhật dở dang. **Sẽ sửa** vì Phase 1 viết lại chính đoạn này (dời validate lên đầu); đây là ngoại lệ có chủ ý.
