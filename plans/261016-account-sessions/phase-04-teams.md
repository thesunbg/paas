# Phase 4 — Max teams + AddMember

File: `internal/server/limits.go`, `internal/server/api_auth.go`, `web/src/lib/api.ts`, `web/src/pages/Settings.svelte`, `web/src/pages/settings/System.svelte`, test `internal/server/teams_test.go`.

## Thay đổi

1. **`TeamLimits`:**
   - Thêm `MaxTeams int \`json:"max_teams"\``, mặc định 3 trong `defaultTeamLimits`.
   - Đưa vào vòng kiểm âm ở limits.go:128.
   - Kiểm rằng setting cũ thiếu key vẫn giữ default.
2. **`handleCreateTeam`:**
   - Khi `limited(r)` đúng và `MaxTeams > 0`, đếm `SELECT count(*) FROM memberships WHERE user_id=$1 AND role='owner'` (query inline cạnh `countTeam`, hoặc thêm một case vào map; chọn cách gọn hơn).
   - Nếu `n >= MaxTeams` thì trả lỗi theo đúng cách các quotaError khác đang trả (xem `checkCounts` cùng call-site của nó để lấy status code), với thông điệp "you can own at most %d teams; ask an administrator for more".
3. **`handleAddMember`**, nhánh email chưa có tài khoản:
   - Caller không phải `IsAdmin` và `allow_signup != "true"` → 403 "registration is closed; ask an administrator to create the account".
   - Caller không phải admin và signup đang bật → áp `signupGuard.take(clientAddr(r), MaxPerIPPerDay)` như `handleRegister`, vượt thì 429.
   - Admin hệ thống thì không bị chặn gì.
4. **Response của `handleAddMember`:** `{"email": u.Email, "role": in.Role}` cộng `temporary_password` nếu vừa tạo user. Không trả user object, dù là user cũ hay user mới.
5. **Web:**
   - `AddMemberResponse` đổi thành `{ email: string; role: Role; temporary_password?: string }`.
   - Settings.svelte:123-124 dùng `r.email`.
   - System.svelte thêm field `{ key: 'max_teams', label: 'Teams per user', hint: 'Owned teams, including the personal one' }`, cộng type `TeamLimits` trong api.ts.

## Test

- `TestCreateTeamLimit`: user thường (đã có team cá nhân nên owned=1) tạo thêm 2 team → 201, 201; team thứ 3 → lỗi quota. Admin tạo team thứ 4 → 201. Khi `max_teams=0` → không giới hạn.
- `TestAddMemberSignupClosed`: `allow_signup` chưa set. User thường tạo team, add email mới → 403, và user đó **không** được tạo (`UserByEmail` → ErrNotFound). Admin hệ thống add email mới → 201, có `temporary_password`.
- `TestAddMemberSignupOpenRateLimited`: `allow_signup=true`, `max_signups_per_ip_per_day=1`. Lần 1 → 201, lần 2 với email mới khác → 429.
- `TestAddMemberNoUserObject`: add user đã tồn tại → 201, body không có key `user`, `name`, `is_admin` hay `totp_enabled`; có `email` và `role`.

## KHÔNG làm

- Luồng invite cần người được mời đồng ý, hay giấu việc email có tồn tại hay không.
- Khoá advisory chống race quota (+1 khi hai request đồng thời): nhất quán với quota project hiện tại.
- Chuẩn hoá email (lower/trim, chặn dạng display-name).
