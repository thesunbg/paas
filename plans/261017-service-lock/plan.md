---
title: Lock theo service không chặn reconcile
status: implemented
created: 2026-10-17
brainstorm: ../reports/brainstorm-261017-service-lock.md
---

# Lock theo service không chặn reconcile

Mục tiêu:
- Build không giữ lock của service.
- `reconcileLoop` không bao giờ đứng chờ một lock. Hệ quả là `MarkStaleNodesOffline` và việc reconcile các service khác vẫn chạy trong lúc có build dài.
- `sleepIdle` không sinh goroutine cho service đang bận.
- Sửa lỗi panic nil sẽ lộ ra khi service bị xoá trong lúc đang build.

## Phase

| # | File | Nội dung | Phụ thuộc |
|---|---|---|---|
| 1 | [phase-01-trylock.md](phase-01-trylock.md) | `TryAdvisoryLock`, `tryLock`, `reconcileOnce`, chia `reconcileService`/`reconcileLocked`, `sleepIdle` dùng try-lock | — |
| 2 | [phase-02-deploy-lock.md](phase-02-deploy-lock.md) | `runDeployment` chỉ lấy lock sau build; đọc lại service dưới lock | 1 (dùng `lock` hiện có, không phụ thuộc cứng) |
| 3 | [phase-03-tests.md](phase-03-tests.md) | Harness NATS nhúng + fake agent; 3 test với Postgres thật | 1, 2 |

Phase 1 và 2 sửa hai file khác nhau, có thể làm song song. Phase 3 phải làm sau cùng.

## Tiêu chí xong (toàn plan)

- Có `PAAS_TEST_DATABASE_URL`: `go test ./internal/server/ -run 'TestReconcile|TestDeploy' -race` xanh. Test chính tạo 4 build đang bị chặn, rồi kiểm tra trong vòng 5s: `reconcileOnce` trả về, node quá hạn chuyển `offline`, instance `lost` của các service đang build bị xoá.
- `go vet ./...` và `go test ./...` (không có DB) đều xanh.

## Quyết định đã chốt

Đã chốt với user trong brainstorm:
- Wake vẫn dùng lock chặn, không dùng try-lock. Đây là chỗ lệch khỏi brief gốc, có chủ ý.
- Service bị xoá trong lúc build thì return. Service bị stop trong lúc build thì deployment chuyển sang `cancelled`.

## KHÔNG làm

- Tách `MarkStaleNodesOffline` ra ticker riêng.
- Sửa `AdvisoryLock` lặp vô hạn khi ctx bị huỷ. Hiện mọi caller truyền `Background`, ghi lại trong brainstorm.
- Sửa chuyện `failDeployment` set `running` cho service vừa bị cho ngủ.
- Đổi lock của DBaaS (`db-*`, `pitr-*`), `moveInstance`, `stopService`. Những chỗ đó không nằm trên hot path của reconcileLoop.
- Thêm cấu hình cho interval, kích thước sem, hay timeout.
