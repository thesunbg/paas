# Phase 5 — Kiểm tra trên node thật

1. Repo thử có `x -> /`, deploy với `root_dir=x` → lỗi "not found". Log có dòng "removed symlink". Trên host không có `/.dockerignore` mới.
2. Repo có `.env.example` nằm trong thư mục symlink `cfg -> /etc/paas` → Inspect không trả nội dung đó về.
3. Service có installation_id, sửa repo thành `https://gitlab.com/...` → deploy fail với lỗi rõ ràng.
4. Deploy repo GitHub private qua installation → OK. Token trong log bị redact.
5. Hai project build Go lần lượt → `buildctl du` cho thấy hai cache record có id khác nhau.
6. Repo `https://<IP private nội bộ>/x.git` → bị từ chối trước clone.

Xong khi: cả 6 bước khớp mô tả, và các service đang chạy redeploy vẫn bình thường.

## Kết quả (2026-10-16)

Redeploy `vietnam-maps` (deployment 2565f54e, commit 9639415) trên build node đã chạy bản e59236a: clone qua IP ghim cùng token scoped OK. Dockerfile sinh ra có `--mount=type=cache,id=ea298a47-…/root/.npm` (project id). Build 2m40s, deployment `active`. Token API tạm đã được xoá. Bước 1–3 và 6 (các ca tấn công) chỉ được cover bằng unit test, chưa chạy trên node.
