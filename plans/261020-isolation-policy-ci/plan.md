---
title: Nhóm I — chính sách cách ly và CI
status: completed
completed: 2026-10-20
created: 2026-10-20
brainstorm: ../reports/brainstorm-261020-isolation-policy-ci.md
---

# Nhóm I — chính sách cách ly và CI

Mục tiêu: (1) deploy không còn root/mật khẩu/host key chưa ghim; (2) sandboxed không rơi về runc; (3) DB public không có đường plaintext; (4) Dockerfile của user không chạm cache hay image của tenant khác.

## Phases

| # | Phase | Phụ thuộc | Tier | File |
|---|---|---|---|---|
| 0 | Xác minh `image-resolve-mode=pull` trên node build | — | main/verifier | [phase-00-verify.md](phase-00-verify.md) |
| 1 | sandboxed fail-closed + tài liệu | — | implementer | [phase-01-sandboxed.md](phase-01-sandboxed.md) |
| 2 | TLS bắt buộc cho DB public | — | implementer | [phase-02-db-tls.md](phase-02-db-tls.md) |
| 3 | `vetDockerfile`: cache rewrite + `# syntax=` | — | implementer | [phase-03-vet-dockerfile.md](phase-03-vet-dockerfile.md) |
| 4 | Ép `image-resolve-mode=pull` | 0, 3 | implementer | [phase-04-resolve-mode.md](phase-04-resolve-mode.md) |
| 5 | deploy.yml + user deploy trên server | — | implementer + main | [phase-05-deploy-ci.md](phase-05-deploy-ci.md) |

Thứ tự chạy: 1 → 2 → 3 → (0) → 4 → 5. Phase 1, 2, 3 độc lập, chạy song song được. **Phase 5 làm TRƯỚC tất cả nếu mật khẩu root chưa đổi.** Phase 0 chạy bất cứ lúc nào có node build.

## KHÔNG làm (cả nhóm)

- userns mặc định cho `standard`: workstream riêng.
- Ký/kiểm chữ ký binary deploy: workstream riêng; deploy key vẫn ≈ root, ghi rõ trong báo cáo.
- STARTTLS MySQL ở edge, `require_secure_transport`, `verify-full`.
- BuildKit riêng mỗi project; cấm hẳn cache mount.
- Vet text `FROM paas.local/` làm cơ chế chính (chỉ là fallback nếu Phase 0 fail).

## Trạng thái (2026-10-20)

| # | Kết quả |
|---|---|
| 0 | Đạt. Xem `../reports/verify-261020-resolve-mode-pull.md`. |
| 1 | Xong, commit e0ba4ec. |
| 2 | Xong, commit 3584d36. Chưa chạy thật: hai nhánh 409 của API bật public port (cần `PAAS_TEST_DATABASE_URL`). Chưa có chỉ báo "blocked" trong dashboard. |
| 3 | Xong, commit c6fedaa. Đã chạy Dockerfile mẫu qua BuildKit trên node thật, cache cách ly đúng. |
| 4 | Xong, commit c6fedaa (cùng build.go với phase 3). Chưa có test riêng cho dòng `--opt image-resolve-mode=pull`. |
| 5 | Xong, commit 04eb42e. Server: user `deploy`, wrapper, sudoers, sshd khoá mật khẩu và root login, deploy key đã xoay. |

## Còn mở

- Mật khẩu root: cần đổi bằng `sudo passwd root` hoặc ở console (không kiểm được từ phiên làm việc).
- Chưa chạy review độc lập cho các commit của nhóm I.
- Deploy key vẫn tương đương root (root chạy paasd do user deploy upload). Điều kiện xoá: kiểm chữ ký binary từ nguồn độc lập.
- Dockerfile user dùng `ONBUILD RUN --mount=type=cache` hoặc mount có dấu nháy bị từ chối (cố ý, fail-closed).
