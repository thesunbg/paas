# Phase 0 — kết quả `image-resolve-mode=pull` (2026-10-20)

Node chính 202.92.6.129, kernel 6.8.0-147, buildkit v0.25.1. Image build ra nằm trong containerd namespace `paas`.

| Thử | mặc định | `pull` |
|---|---|---|
| `FROM paas.local/<seed>` | OK | FAIL (`Head "https://paas.local..."`) |
| `COPY --from=paas.local/<seed>` | OK | FAIL |
| `RUN --mount=type=bind,from=paas.local/<seed>` | OK | FAIL |
| `FROM ${A}/${B}` ghép từ ARG | OK | FAIL |
| `FROM node:22-alpine` | 1.8s (lạnh) | 0.4s (có cache, lần 2 cũng 0.4s) |
| `FROM golang:1.24-alpine` | — | 1.3s (lạnh) |

Kết luận: Phase 4 dùng `image-resolve-mode=pull`, không cần fallback vet text. Base image thật build bình thường, không thấy chậm thêm. Chưa đo rate limit Docker Hub (HEAD manifest không tính vào quota theo tài liệu Docker, chưa kiểm).

Đã dọn: mọi image `paas.local/phase0-*` và file tạm trên node.
