# Phase 1 — Lọc symlink sau clone

File: `internal/build/build.go`, test mới `internal/build/symlink_test.go`.

## Thay đổi

1. `sanitizeSymlinks(src string, log Logger) error`. Trước tiên `base, _ := filepath.EvalSymlinks(src)` (WorkDir có thể nằm dưới một symlink). Sau đó `WalkDir(src)`: với mỗi entry là symlink, gọi `t, err := filepath.EvalSymlinks(p)`; nếu `err != nil` (link treo, vòng lặp) hoặc `filepath.Rel(base, t)` bắt đầu bằng `..` → `os.Remove(p)` rồi log `removed symlink ... (points outside the repository)`. `WalkDir` không đi theo symlink thư mục nên không đệ quy ra ngoài.
   - Thứ tự: một link chỉ có thể trỏ ra ngoài qua chính nó hoặc qua link khác. Link trung gian trỏ ra ngoài cũng bị xoá, và EvalSymlinks của link sau đó lỗi → cũng bị xoá. Walk một lần là đủ, không phụ thuộc thứ tự, vì tiêu chí được đánh giá trên FS lúc chưa xoá gì. Nếu implementer thấy có ca nào cần walk lần hai thì ghi lý do.
2. Gọi trong `checkout()` ngay sau `os.RemoveAll(dest/.git)`. Cả Build lẫn Inspect đều đi qua đây.
3. Đổi `removeSymlinks` (static output) thì **không** đụng tới, vì đó là output của build, một chuyện khác.
4. Không thêm check nào ở build.go:85 hay :358. Comment một dòng ở `checkout`: "after this, nothing under dest resolves outside dest".

## Test

- Repo tạm (thư mục, không cần git) có: `x -> /`, `y -> ../../etc`, `.dockerignore -> /nonexistent/zz` (treo), `a -> b` với `b/` là dir thật, `c -> b/file`. Sau sanitize: x, y, .dockerignore bị xoá; a, c còn.
- Build đi qua `checkout` với file:// (AllowLocalGit) và `root_dir: x` → lỗi `root directory "x" not found`. Dùng git thật nếu `build_integration_test.go` đã có sẵn khung; không thì test hàm `sanitizeSymlinks` cộng đoạn chọn root (tách ra hàm nhỏ nếu cần).
- `ensureDockerignore` sau sanitize không tạo `/nonexistent/zz`.

## KHÔNG làm

- `core.symlinks=false`, chặn mọi symlink.
- Sửa `findCompose`/`exampleEnv`/`detect`: đã an toàn nhờ bước 2.
