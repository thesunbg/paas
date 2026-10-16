---
title: Tài khoản và phiên đăng nhập
status: implemented
created: 2026-10-16
brainstorm: ../reports/brainstorm-261016-account-sessions.md
---

# Tài khoản và phiên đăng nhập

Mục tiêu: thu hồi credential khi mật khẩu hoặc 2FA thay đổi; token trong URL chỉ dùng được cho SSE; 2FA không bị brute-force hay dùng lại mã; lockout không khoá được nạn nhân; không lách được quota bằng cách tạo nhiều team hay đăng ký vòng qua AddMember.

## Phases

| # | Phase | Phụ thuộc | Tier | File |
|---|---|---|---|---|
| 0 | Harness test handler với PG thật | — | implementer | [phase-00-harness.md](phase-00-harness.md) |
| 1 | Thu hồi session/token + access_token chỉ cho SSE | 0 | implementer | [phase-01-sessions.md](phase-01-sessions.md) |
| 2 | 2FA: giới hạn thử + chống dùng lại mã | 0 | implementer | [phase-02-totp.md](phase-02-totp.md) |
| 3 | Lockout email+IP | 0 | implementer | [phase-03-lockout.md](phase-03-lockout.md) |
| 4 | Max teams + AddMember | 0 | implementer | [phase-04-teams.md](phase-04-teams.md) |

Phase 1 đến 4 đều sửa `api_auth.go`. Giao **một** implementer làm tuần tự 1→4, commit sau mỗi phase. Đừng chạy song song: các agent sẽ ghi đè lên nhau trong cùng một file. Tổng thay đổi ước ~300 dòng cộng test.

## KHÔNG làm

- Bảng lockout trong DB (đếm chung giữa các instance): là đổi schema, và suất một cột đã dành cho `totp_last_step`.
- Luồng invite (cần user đồng ý trước khi vào team, giấu việc email có tồn tại hay không): cần bảng mới.
- Interface hoá `Store` hay thêm embedded-postgres.
- Bỏ hẳn `?access_token`: docs công bố tham số này cho client ngoài.
- Thu hồi session khi *bật* 2FA; validate email dạng `"Name <x@y>"`.
- Đổi TTL session hay rotate session định kỳ.

## Tiêu chí xong (toàn plan)

- Mỗi mục trong phạm vi có ít nhất một test handler, liệt kê ở từng phase.
- Có `PAAS_TEST_DATABASE_URL`: `go test ./internal/server/... ./internal/totp/... ./internal/store/...` xanh. Không có: các test DB skip, phần còn lại xanh.
- CI chạy được các test DB (service postgres:16).
- `go vet ./...` sạch; `cd web && npm run check` (nếu có script) sạch.
- Schema chỉ đổi đúng `011_totp_last_step.sql`.

## Rủi ro chính

1. Đổi mật khẩu sẽ xoá mọi API token của user, nên CI phía người dùng sẽ hỏng. Cần release note.
2. Dựng `Server` tối thiểu cho test có thể vướng các field mà route mux cần lúc đăng ký. Phase 0 xử lý; nếu phải sửa code sản xuất (tách hàm đăng ký route ra khỏi `New`) thì giữ ở mức tối thiểu.
3. Lockout vẫn in-memory theo từng instance. Ghi trong comment ở `attemptGuard`, kèm điều kiện xoá: khi counter chuyển vào bảng.
