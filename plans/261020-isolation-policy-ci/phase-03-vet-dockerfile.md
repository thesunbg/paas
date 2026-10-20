# Phase 3 — `vetDockerfile`: cache rewrite + `# syntax=`

File mới: `/home/hwng/github_projects/paas/internal/build/vet.go` + `vet_test.go`; gọi từ `internal/build/build.go` (~:111, sau `detect.Detect`, chỉ khi `plan.UserDockerfile != ""`, trước khi chạy buildctl). Không có BuildKit parser trong go.mod: quét thủ công.

## Việc
`vetDockerfile(path, scope string) error` ghi đè file tại chỗ (bản clone trong work dir):
1. Đọc directive đầu file: `# syntax=` phải khớp `docker/dockerfile(:|@)...`, nếu không → lỗi. `# escape=` khác `\` → lỗi.
2. Nối dòng tiếp `\`, nhận instruction `RUN`, tách các cờ `--mount=` trước phần lệnh (bỏ qua heredoc body).
3. `type=cache`: có `id=X` → `id=<scope>/X`; không có → `id=<scope><target>` (target từ `target=`/`dst=`/`destination=`). Giữ nguyên thứ tự và các option khác (`sharing=`, `mode=`...).
4. Fail-closed: `--mount` có `type=` không literal (`$`), hoặc đếm `type=cache` toàn file ≠ số đã viết lại → lỗi nêu số dòng và lý do.
5. `scope == ""` (control plane cũ) → bỏ qua rewrite, giữ hành vi `Options.cache` hiện tại; comment nêu điều kiện xoá nhánh.
6. Test bảng: không id, có id, nhiều mount một dòng, nhiều dòng, `type=$T`, `# syntax=evil`, `# syntax=docker/dockerfile:1`, `# escape=\``, cache mount trong comment/heredoc, golden Dockerfile thật (Go, npm, pip).

## Xong khi
`go test ./internal/build/...` xanh; mọi `type=cache` ra khỏi hàm đều có `id=<scope>`.

## KHÔNG làm
Không BuildKit riêng mỗi project; không cấm cache mount; không dùng regex đoán khi gặp cú pháp lạ (từ chối).
