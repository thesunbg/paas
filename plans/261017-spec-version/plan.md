---
title: Cột version + compare-and-swap cho spec service và options database
status: implemented
created: 2026-10-17
---

# Cột version + compare-and-swap

## Lỗi

Build đọc `svc` lúc bắt đầu deploy, chạy vài phút, rồi ghi cả spec cũ về (`SetServiceSpec`). Người dùng đổi replicas trong lúc build thì bị ghi đè. Cùng mẫu read-modify-write nằm ở `UpdateServiceConfig` và `UpdateDatabaseOptions` (scale, prisma, HA, pooler, quota, metrics, PITR...).

## Quyết định

- Migration `012_spec_version.sql`: `services.version` và `databases.options_version` (`databases.version` đã là version của engine), mặc định 1.
- Store chỉ còn hai hàm ghi config: `UpdateService(id, fn)` và `UpdateDatabaseOptions(id, fn)`. Mỗi hàm đọc, chạy `fn`, rồi `UPDATE ... WHERE version=$v`; không khớp thì đọc lại và chạy lại `fn` (tối đa 5 lần), hết lượt trả `ErrConflict`. Chỉ ghi config mới tăng version; status, node, deployment, last_request_at thì không.
- Mỗi caller thành closure chỉ áp phần thay đổi của mình lên giá trị mới đọc, và kiểm tra điều kiện (vd. `Port == 8080`, volumes thì replicas <= 1) trên giá trị đó. Sau khi ghi, caller làm mới `svc` trong bộ nhớ từ giá trị trả về.
- HTTP: GET một service/database trả `ETag: "<version>"`. `PATCH /services/{id}` và `POST /databases/{id}/public` nhận `If-Match`; lệch version thì `ErrStale` (không retry) và HTTP 412. Không có header thì merge phía server như cũ. `ErrConflict` thành 409.
- UI: `updateService(id, input, version?)` và `databasePublic(..., version?)` gửi `If-Match`; gặp 412 thì hiện lỗi và reload.

## KHÔNG làm

- `jsonb_set` theo từng field: không giữ được ràng buộc chéo (volumes thì 1 replica).
- `SELECT FOR UPDATE`: closure có network IO (`checkInstallation`).
- Đụng `s.lock` / advisory lock.
- Version cho status/node/deployment.
- Helper CAS chung cho bảng bất kỳ.

## Ghi chú

- Closure của database chỉ thấy `DBOptions`, nên `If-Match` của `/public` so với version đọc ngay sau khi giữ lock `db-public-port`; closure bị gọi lần hai nghĩa là version đã đổi, trả `ErrStale`. Điều kiện xoá: đưa version vào closure nếu có form thứ hai gửi `If-Match`.

## Tiêu chí xong

Test với Postgres thật (`PAAS_TEST_DATABASE_URL`): closure "build" xen kẽ một lần scale thì giữ cả replicas lẫn language (closure chạy đúng 2 lần, version +2); tương tự cho options; retry hết lượt trả `ErrConflict`; PATCH với If-Match cũ trả 412, khớp trả 200, không header trả 200.
