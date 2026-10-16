# Phase 4 — Cache id theo project

File: `internal/proto/*.go` (BuildRequest), `internal/server/deploy.go`, `internal/build/build.go`, `internal/detect/detect.go`, `internal/detect/detect_test.go`.

## Thay đổi

1. `proto.BuildRequest.ProjectID string \`json:"project_id"\``. `deploy.go` gán `svc.ProjectID`.
2. `detect.Options.CacheScope string`. build.go truyền `req.ProjectID`.
3. Một helper duy nhất trong detect, ví dụ `func (o Options) cache(target string) string`, trả `--mount=type=cache,target=<t>` hoặc `--mount=type=cache,id=<scope><t>,target=<t>` khi scope khác rỗng. Thay toàn bộ các dòng `type=cache` (315, 497, 499, 657, 709, 754, 796, 833) bằng helper này. Option phải truyền được tới các hàm sinh Dockerfile; nếu chúng chưa nhận `opt` thì thêm tham số, đừng dùng biến global.
   - Comment ở nhánh scope rỗng: "paasd older than this change sends no project_id; drop this branch once all control planes do."
4. Sau khi sửa, `grep -n 'type=cache' internal/detect/detect.go` chỉ còn khớp trong helper.

## Test

- Với mọi fixture đang có trong `TestDetect` (đủ các ngôn ngữ): Detect với `CacheScope: "p1"` → mọi dòng chứa `type=cache` đều chứa `id=p1/`. Assert bằng vòng lặp, đừng chép từng dòng.
- Scope rỗng → không có `id=` (giữ hành vi cũ).

## KHÔNG làm

- Đụng tới Dockerfile do user viết.
- Hash/HMAC project ID: UUID ngẫu nhiên đã không đoán được.
