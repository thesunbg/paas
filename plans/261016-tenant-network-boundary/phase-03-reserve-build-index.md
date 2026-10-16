# Phase 3 — Dành idx 1022 cho build

File: `internal/network/network.go`, `internal/network/dns_test.go`, `internal/store/migrations/010_reserve_build_net.sql`.

## Thay đổi

1. `network.go`:
   ```go
   // MaxProjects bounds tenant project indexes (1..MaxProjects-1). BuildNetIndex is the
   // BuildKit network; slot 1023 is infrastructure.
   MaxProjects   = 1022
   BuildNetIndex = 1022
   ```
   `network()` vẫn từ chối `idx >= MaxProjects`. Bridge build không đi qua `network()` (Phase 4 tự sinh cấu hình CNI).
2. `dns_test.go:53`: assert `subnetFor(base, 1, BuildNetIndex)` có prefix `10.101.255.128`. Thêm assert `subnetFor(base,1,MaxProjects-1)` có prefix `10.101.255.64`.
3. Migration `010_reserve_build_net.sql`:
   ```sql
   ALTER TABLE projects ADD CONSTRAINT projects_net_index_tenant CHECK (net_index < 1022);
   ```
   Migration fail nếu đã có project vượt ngưỡng, và đó là tín hiệu đúng (Phase 0c). Theo cách migration hiện có được chạy (xem `internal/store`), thêm file là đủ.

## KHÔNG làm

- Không tái sử dụng `net_index` đã xoá (giới hạn SERIAL có từ trước, không thuộc plan này).
- Không thêm kiểm tra trong handler. Constraint của DB là một nguồn sự thật duy nhất.

## Xong khi

`go test ./internal/network/... ./internal/store/...` xanh. Migration chạy được trên DB dev.
