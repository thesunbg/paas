# Brainstorm 2026-10-20 — Phiên bản và vòng đời database

## Vấn đề
Catalog DB (`internal/databases/engines.go`) chỉ pin major, tag trôi (`postgres:17-alpine`). Hậu quả:
1. Không biết chính xác đang chạy bản nào (không digest, `serverVersion()` chỉ để hiển thị, cache 10 phút).
2. Không có major upgrade. "Upgrade" trong code = shared→dedicated (`dbops.go:534`).
3. Không có EOL/trạng thái. Version cứng trong Go, vẫn cho tạo mới trên bản sắp/đã EOL.
4. Engine `mysql` thực ra là MariaDB (image `mariadb`, CLI `mariadb`, env `MARIADB_*`, ~15 chỗ hardcode).
5. Postgres chạy alpine (musl): collation lệch glibc, import từ PG thường có thể sai thứ tự index.

Bối cảnh: chưa chạy production → đổi image/data model lúc này gần như miễn phí.

## Đã chốt với user
- Làm cả 4 hướng: biết chính xác đang chạy gì, major upgrade, EOL, tách engine.
- Tách `mysql` (MySQL 8.4 LTS) và `mariadb`; DB cũ đổi nhãn bằng migration.
- Major upgrade = phương án A (dump/restore sang server mới, giữ bản cũ để rollback). Chấp nhận downtime.
- Postgres sang Debian (glibc) ngay. DB dev hiện có tạo lại.
- Không thêm engine/biến thể mới đợt này.

## Phương án major upgrade đã xét
| | Kết luận |
|---|---|
| A. Dump/restore sang server mới | **Chọn.** `upgradeDatabase` đã có chuỗi backup→tạo server→restore→đổi link→drop cũ→restart. Chỉ đổi đích: shared dùng `ensureSharedServer(engine, newVersion)`, dedicated dùng `createDedicatedServer` với version mới. Chạy cho cả 4 engine |
| B. `pg_upgrade --link` | Loại: cần binary 2 version trong 1 image, chỉ Postgres, rollback khó |
| C. Logical replication zero-downtime | Loại: chỉ Postgres, quá nặng. Làm lại khi có khách đòi |

## Giải pháp
1. **Postgres Debian.** `ImageFor` bỏ hậu tố `-alpine` cho postgres. Grep cho thấy HA/PITR/dbaas không phụ thuộc `apk`/`su-exec`. Vẫn phải chạy lại test HA và PITR.
2. **Catalog có EOL.** `Versions []string` → `[]Version{Name, EOL}`, vẫn trong Go. Trạng thái tính từ ngày: `ok` / `ending` (<6 tháng) / `eol`. Tạo mới trên `eol` → 409. DB đang chạy `ending`/`eol` → banner dashboard + notify (kênh sẵn có). Hiện trong `/api/v1/sizes`.
3. **Pin digest.** Agent đọc digest sau pull (`runtime.EnsureImage`), server ghi vào spec của server DB. UI hiện `17.6 (sha256:…)`. Nút "Cập nhật minor": pull lại tag, ghi digest mới, restart có kiểm soát; shared server phải cảnh báo ảnh hưởng mọi DB logic trên đó.
4. **Major upgrade.** Endpoint kiểu `POST /databases/{id}/version {version}`: generalize `upgradeDatabase`. Chỉ cho tăng version. Backup trước, giữ server/DB cũ N ngày để rollback.
5. **Tách MySQL/MariaDB.** Tham số hoá tên binary (`mysql`/`mariadb`, `mysqldump`/`mariadb-dump`, `mysqld`/`mariadbd`) và env (`MYSQL_*`/`MARIADB_*`) thay vì copy code. Migration đổi `engine='mysql'` cũ thành `mariadb`. Cập nhật `store/mysql_no_public_test.go`, proxy mode `plain`, template, UI, API.

## KHÔNG làm (và vì sao)
- Catalog trong DB / UI quản trị version: một người vận hành, release một binary là đủ.
- Valkey: Redis 8 đã về AGPLv3, không ai đòi; thêm sau tốn một entry.
- pgvector/PostGIS (biến thể): chưa có người dùng; Debian là điều kiện cần và đã làm ở bước 1.
- Auto-update minor: restart DB ngoài ý muốn tệ hơn chạy minor cũ vài tuần.
- Zero-downtime major upgrade: xem phương án C.

## Thứ tự (mỗi bước merge riêng)
1. Postgres Debian + chạy lại test HA/PITR.
2. Catalog EOL + trạng thái + chặn tạo mới + banner/notify.
3. Pin digest + version thật + nút cập nhật minor.
4. Major upgrade.
5. Tách MySQL/MariaDB (tốn công nhất).

## Rủi ro
- Ngày EOL tôi nhớ từ trí nhớ (PG 14 11/2026, MariaDB 10.6 7/2026, Mongo 6.0 7/2025…): **xác minh trên endoflife.date trước khi ghi vào code**. Thêm ngày vào catalog là việc bảo trì định kỳ mỗi release.
- Major upgrade dump/restore: DB lớn → downtime dài; cần timeout và dung lượng đĩa cho file dump (tương tự backup hiện tại).
- Mongo: dump/restore bỏ qua ràng buộc FCV tuần tự của upgrade tại chỗ nhưng cần kiểm tra dump 6.0→8.0 thực tế.
- Digest phụ thuộc agent đọc được sau pull; node cũ không báo digest → hiển thị "không rõ" thay vì lỗi.
- MySQL 8.4: `caching_sha2_password` mặc định; client cũ có thể cần cấu hình. Ghi vào docs.

## Tiêu chí thành công
- Tạo DB ở bản `eol` bị từ chối; bản `ending` hiện cảnh báo.
- Mỗi DB hiển thị version đầy đủ + digest; cập nhật minor đổi digest, dữ liệu còn nguyên.
- Nâng PG 16→17 và MariaDB 10.11→11.4 trên shared và dedicated: dữ liệu đủ, link tự trỏ server mới, rollback trong N ngày.
- `engine=mysql` chạy MySQL 8.4 thật (`SELECT VERSION()`); `mariadb` chạy như cũ.
- Test HA/PITR xanh trên image Debian.
