---
title: Hộp cát cho build
status: implemented
created: 2026-10-16
brainstorm: ../reports/brainstorm-261016-build-sandbox.md
---

# Hộp cát cho build

Mục tiêu: nội dung repo và URL do tenant đặt không đọc/ghi được file trên host của build node, không lấy được token của repo khác, không đầu độc được cache của tenant khác, không dùng clone để chạm tới IP private.

Ý chính: **sửa dữ liệu ở nguồn, không vá ở từng chỗ dùng.** Sau clone, `src` được lọc symlink một lần. Token được mint scoped để GitHub tự thực thi quyền. Cache id sinh ra từ một helper duy nhất.

## Phases

| # | Phase | Phụ thuộc | Tier | File |
|---|---|---|---|---|
| 0 | Xác minh git trên build node | — | main | [phase-00-verify.md](phase-00-verify.md) |
| 1 | Lọc symlink sau clone | — | implementer | [phase-01-symlinks.md](phase-01-symlinks.md) |
| 2 | Clone guard (https + IP public + ghim) | 0 | implementer | [phase-02-clone-guard.md](phase-02-clone-guard.md) |
| 3 | Token GitHub scoped theo repo | — | implementer | [phase-03-scoped-token.md](phase-03-scoped-token.md) |
| 4 | Cache id theo project | — | implementer | [phase-04-cache-id.md](phase-04-cache-id.md) |
| 5 | Kiểm tra trên node thật | 1–4 | main | [phase-05-e2e.md](phase-05-e2e.md) |

Phase 1, 3, 4 chạy song song được ngay. Phase 2 chờ kết quả Phase 0. Phase 1 và 2 cùng sửa `checkout()` trong build.go: giao cho cùng một implementer, hoặc làm tuần tự.

## KHÔNG làm

- Check EvalSymlinks/Rel ở từng điểm (build.go:85, :358, Dockerfile, exampleEnv): Phase 1 làm chúng thừa. Giữ nguyên `filepath.Clean("/"+x)` hiện có.
- `core.symlinks=false`: làm hỏng symlink nội bộ.
- Cách ly cache của Dockerfile do user viết (id tự chọn): cần cách ly ở cấp BuildKit, để sau.
- Ràng `GitTokenEnc` (token cá nhân) theo host: token của chính user, không phải lỗ hổng xuyên tenant.
- Gọi `Repos()` để kiểm repo: mint scoped đã để GitHub tự kiểm.
- Validate URL repo lúc tạo service ở API: có thì UX đẹp hơn, nhưng chặn ở build/mint đã đủ đúng. Thêm khi có người phàn nàn.
- Fallback resolve-only khi git <2.37: fail-closed.
- IPv6 allowlist, hay danh sách host git được phép cấu hình được: chưa có người dùng.

## Tiêu chí xong (toàn plan)

Xem từng phase cộng Phase 5. Tóm tắt: có test cho `x -> /`, cho repo không phải GitHub kèm installation_id, cho cache id trong Dockerfile sinh ra, cho clone http/loopback. `go test ./internal/build/... ./internal/detect/... ./internal/github/... ./internal/server/...` xanh.

## Rủi ro chính

1. git trên node <2.37 → Phase 2 không có `curloptResolve`. Phase 0 chặn sớm.
2. Service đang có installation_id cộng repo ngoài GitHub, hoặc owner lệch, sẽ build fail. Đó là đúng ý; cần release note.
3. paasd cũ + agent mới: `ProjectID` rỗng → không gắn cache id (hành vi cũ), có comment kèm điều kiện xoá.
