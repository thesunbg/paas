# Phase 0 — Xác minh `image-resolve-mode=pull`

Không sửa code. Quyết định Phase 4 đi theo `pull` hay fallback vet text. Chạy trên node build thật.

## Việc
1. Build image `paas.local/x:1` (qua `paas` bình thường hoặc `buildctl ... --output type=image,name=paas.local/x:1,unpack=true`).
2. Dockerfile `FROM paas.local/x:1`: build với mặc định → kỳ vọng thành công; build với `--opt image-resolve-mode=pull` → kỳ vọng fail (không resolve được registry).
3. Cùng thử `COPY --from=paas.local/x:1` và `RUN --mount=type=bind,from=paas.local/x:1,...` với `pull`.
4. Base thật (`node:22-alpine`, `golang:1.24`) vẫn build được với `pull`; ghi thời gian build có/không `pull` (HEAD manifest) và có chạm rate limit không.
5. Thử `FROM ${A}/${B}` với build arg ghép thành `paas.local/x:1`: phải fail với `pull`.

## Tiêu chí xong
Bảng kết quả ghi vào `plans/reports/`. Mục 2, 3, 5 fail đúng → Phase 4 dùng `pull`. Không → Phase 4 dùng fallback (xem file đó).

## KHÔNG làm
Không đổi code build trong phase này; không đo ngoài các mục trên.
