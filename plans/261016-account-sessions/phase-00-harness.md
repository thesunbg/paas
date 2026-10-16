# Phase 0 — Harness test handler

File: `internal/server/testserver_test.go` (mới), `.github/workflows/ci.yml`; nếu buộc phải thì thêm `internal/server/api.go` hoặc `server.go`.

## Thay đổi

1. `newTestServer(t) (*Server, http.Handler)`:
   - Đọc `PAAS_TEST_DATABASE_URL`, chưa set thì `t.Skip`.
   - Kết nối DB admin, `CREATE DATABASE paas_test_<random>`, `store.Open` vào DB mới rồi `Migrate`.
   - `t.Cleanup`: đóng store, `DROP DATABASE … WITH (FORCE)`.
   - Một DB cho mỗi test là được; nếu quá chậm thì một DB cho mỗi package cộng `TRUNCATE` giữa các test, implementer tự chọn và ghi lý do.
2. Dựng `&Server{store, box, cfg}` tối thiểu, với `box` sinh từ key ngẫu nhiên (xem package `secrets`), rồi lấy `http.Handler` thật để đi qua mux và `authed`. Đọc cách `api.go` đăng ký route (`h(...)`). Nếu việc đăng ký đòi NATS hay runtime thì tách phần đăng ký route thành một hàm gọi được mà không cần `New()`, và chỉ thay đổi tối thiểu.
3. Helpers, chỉ những thứ Phase 1–4 thật sự dùng:
   - `mkUser(t, email, pw, admin) *store.User`;
   - `login(t, h, email, pw, ip) (status int, token string)`, đặt `RemoteAddr` theo `ip`;
   - `do(t, h, method, path, token, body) *httptest.ResponseRecorder`, dùng header Bearer (với mutation, `authed` bỏ qua kiểm tra CSRF khi có header Authorization).
4. Mỗi lần gọi `newTestServer` reset `loginGuard` và `signupGuard`, vì đó là biến cấp package.
5. CI: job Go thêm `services: postgres: image: postgres:16`, kèm env `PAAS_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable` và health check.
6. Thêm một dòng comment trên target `test` trong Makefile, chỉ cách chạy local: `podman run -d -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:16` rồi export biến.

## Test

- `TestHarnessSmoke`: tạo user, login, gọi `GET /api/v1/me` bằng token → 200.

## KHÔNG làm

- Interface hay fake cho Store.
- Thêm dependency mới (testcontainers, embedded-postgres).
- Helper "cho sau này" mà chưa phase nào dùng.
