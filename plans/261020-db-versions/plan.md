---
title: Phiên bản và vòng đời database — Postgres glibc, catalog EOL, pin digest, major upgrade một chiều, tách MySQL/MariaDB
status: code-complete (chưa kiểm chứng trên node thật)
created: 2026-10-20
depends_on: 261017-db-ha
---

# Phiên bản và vòng đời database

Lý do, phương án đã loại, rủi ro: `plans/reports/brainstorm-261020-db-versions.md`.
Red-team và validate 2026-10-20 đã áp vào plan này (hai mục cuối). Chưa có production nên được đổi image và data model. Mỗi phase merge riêng. Số migration gán lúc merge (dưới đây chỉ là tên, không phải số).

**Thứ tự thực hiện: Phase 1 → 2 → 5 → 3 → 4** (số phase giữ nguyên để tham chiếu chéo không vỡ). Phase 5 (tách MySQL/MariaDB) làm trước 3 và 4 để pin digest và major upgrade chỉ viết một lần với tên engine `mariadb`/`mysql` cuối cùng; mọi nhắc tới "MariaDB" ở Phase 3/4 (test, slug, setting) là engine `mariadb` sau Phase 5. Phase 4 phụ thuộc Phase 2 (catalog) và Phase 5.
**Môi trường kiểm chứng:** cluster dev 2 node (đủ cho HA, switchover, reschedule digest). Test tay ghi kết quả vào `plans/reports/`.

## Sự thật đã xác minh trong code (đừng đoán lại)

- `Engines` (`internal/databases/engines.go`) giữ `Versions []string`, `LegacyVersion`, `Image`. `ImageFor` ghép tag; `""` = image legacy.
- Version nằm ở `databases.version`; shared server mỗi version một service `shared-<engine>[-<ver>]` (`ensureSharedServer`, `dbops.go:221`). Mật khẩu root của shared server lưu ở setting `shared_<engine>_root` (`sharedRootPassword`, `dbops.go:158`): **cả slug lẫn setting key đều lấy từ tên engine**. Image còn nằm cứng trong `Spec.Image` và `Source.Image` của service server.
- `runtime.EnsureImage` (`runtime.go:161`) **chỉ pull khi image chưa có trên node**. Tag không bao giờ tự làm mới; mỗi node đóng băng ở lần pull đầu. Primary và standby HA trên 2 node có thể chạy 2 minor khác nhau.
- Rollout của service có volume **dừng instance cũ trước rồi mới start instance mới** (`deploy.go:501-504`): luôn có downtime, và pull lỗi thì DB sập.
- `PGDATA` đã được set tường minh (`engines.go:162`).
- `upgradeDatabase` (`dbops.go:534`) hiện **đổi con trỏ trước khi restore**: `createDedicatedServer` gọi `SetDatabaseServer` ngay khi tạo (mọi mode ≠ shared), `upgradeDatabase` gọi `SetDatabaseMode` trước `Restore`. `createDedicatedServer` chỉ thêm hậu tố `-db` vào slug khi mode là shared, nên dedicated→dedicated đụng unique slug. Handler (`api_databases.go:243`) chạy trong goroutine, không giữ lock; `resumeProvisioning` (`server.go:420`) khi restart chỉ đặt `upgrading` về `ready`, không dọn gì. Phase 4 **không thể** dùng lại nguyên văn.
- `databases.Restore` không có case Redis (`engines.go`: `restore is not supported for redis yet`).
- Backup giữ 7 bản mới nhất mỗi DB (`OldBackups(ctx, db.ID, 7)`, `dbops.go:467`). `volume_gc` (migration 015) được ghi cùng transaction xoá row service; `volumeGCTick` (`dbaas_failover.go:444`) xoá volume theo `delete_after`.
- `tcpMode(engine)` (`dbaas.go:30`) mặc định `plain`: engine không biết thì public port vẫn bị chặn.
- HA/PITR chạy `sh -c` (`pgPrelude`, `dbaas_ha.go`), `pg_basebackup`, `chown postgres:postgres`. Không dùng `apk`/`su-exec`.
- Chỗ gọi tên engine `mysql`: `engines.go` (15 hàm), `dbaas_metrics.go:125`, `studio.go:89`, `prisma.go:58`, `templates/compose.go:60,689`, `templates/templates.go:64` (WordPress), `proxy/tcp.go`, migration `017`, test `store/mysql_no_public_test.go`, web (`Database.svelte`, `NewDatabaseDialog.svelte`, `Import.svelte`, `Project.svelte`, `api.ts`), `cmd/paas/main.go`, `deploy.go:287,351` (`Spec.Language` đọc cho tuning), `api_templates.go`, `dbops.go:123,223`.

## Ngày EOL đã xác minh trên endoflife.date (2026-10-20)

| Engine | Version → EOL |
|---|---|
| postgres | 14: 2026-11-12 · 15: 2027-11-11 · 16: 2028-11-09 · 17: 2029-11-08 · 18: 2030-11-14 |
| mariadb | 10.6: 2026-07-06 (**đã EOL**) · 10.11: 2028-02-16 · 11.4: 2029-05-29 · 11.8: 2028-06-04 |
| mysql | 8.4: 2032-04-30 |
| mongo | 6.0: 2025-07-31 (**đã EOL**) · 7.0: 2027-08-31 · 8.0: 2029-10-31 |
| redis | 7.2: 2029-12-01 · 7.4: 2029-12-01 · `8`: **không có EOL** |

Redis `8` là tag trôi theo cycle mới nhất: để EOL trống và UI không hiện trạng thái cho nó. Cycle 8.0 (EOL 2026-12-01) không áp cho tag `8`. Chỉ dùng bảng này, không dùng trí nhớ.

## Phase 1 — Postgres sang Debian (glibc)

- `ImageFor`: bỏ hậu tố `-alpine` của postgres (`postgres:<v>`), và `Engines[Postgres].Image` thành `postgres:17`.
- Cập nhật `TestVersions`.
- DB dev đang có phải tạo lại (đã chốt với user): dữ liệu alpine không an toàn trên glibc. Không viết migration dữ liệu và không viết guard chống gắn nhầm volume (chưa có production).
- **Xong khi**: `go test ./...` xanh, và chạy lại trên Debian thật: tạo DB dedicated PG 17 và 18, HA (standby + switchover), PITR (restore tới thời điểm), import từ một PG ngoài, PgBouncer. `PGDATA` PG 18 nằm trên volume (xoá container, tạo lại, dữ liệu còn).
- **Không đổi**: Redis alpine, Mongo, MariaDB.

## Phase 2 — Catalog có EOL

**Data**
- `Versions []string` → `Versions []Version{Name string; EOL time.Time}` (zero = không có EOL). Điền theo bảng trên.
- Hàm thuần `Status(v Version, now time.Time) string`: `eol` nếu `now ≥ EOL`; `ending` nếu còn < 180 ngày; còn lại `ok`; EOL zero → `ok`. Hằng `EndingWindow` một chỗ.
- **Xoá** `LegacyVersion` và nhánh `version == ""` trong `ImageFor`, `DisplayVersion`, `ensureSharedServer`. Migration `db_version_backfill`: `UPDATE databases SET version = <default của engine> WHERE version = ''` chỉ để thoả `version <> ''` (CHECK). Nhãn này **không đáng tin** với DB cũ (tag đóng băng theo node), nhưng DB dev sẽ được tạo lại; phase 3 hiển thị version thật từ `serverVersion()`, và Phase 4 so thứ tự theo catalog, không dựa vào nhãn đoán.

**API**
- `GET /sizes`: mỗi version trả `{name, eol, status}`. Form tạo chỉ liệt kê `ok`/`ending`; `eol` ẩn khỏi form nhưng vẫn hiện ở trang DB đang dùng nó.
- `createDatabase`: version tra cứu **chính xác** trong `Engines[engine].Versions` (không ghép chuỗi người dùng nhập vào tag). `eol` → 409 "version X reached end of life on D". `ending` cho tạo, trả `warning`.
- Quy tắc EOL áp ở hàm duy nhất dựng server spec (`ServerSpec`/`createDatabase`), không rải ở từng handler; tạo qua template/compose/PITR restore/HA standby đều đi qua đó. Kiểm tra `defaultVersion` của mọi template không rơi vào `eol`.
- Database detail trả `version_status`.

**Banner và notify**
- Dashboard: banner ở `Database.svelte`, badge ở `Project.svelte` khi `ending`/`eol`.
- Event mới `database.eol` (thêm vào `notificationEvents`). Maintenance loop gửi **một lần** mỗi (database, trạng thái). Dedup bằng cột riêng `databases.eol_notified text` (không đặt trong `options`, vì `options_version` là khoá lạc quan của lần lưu người dùng, ghi từ loop sẽ gây 412/mất cập nhật).

**Xong khi**: bảng test `Status` (ranh giới đúng ngày); tạo MariaDB 10.6 và Mongo 6.0 → 409; PG 14 → 201 + warning; version lạ (`17@sha256:…`, `../x`) → 400; `GET /sizes` đúng; notify bắn một lần, không lặp sau restart và không đụng `options_version`.

## Phase 3 — Pin digest thật và cập nhật minor

**Vấn đề** (xem trên): minor đóng băng theo node; "digest" chỉ để quan sát thì redeploy/reschedule vẫn trôi.

**Pin thật**
- Agent báo `ContainerStatus.ImageDigest` = **digest manifest của nền tảng** (`linux/<arch>`), không phải digest của index đa kiến trúc (hai node cùng binary phải ra cùng digest). Lưu ở `instances.image_digest` (migration `instance_image_digest`).
- Khi server nhận digest lần đầu của service DB mà `Spec.Image` còn là tag, ghi lại `Spec.Image = <repo>:<tag>@sha256:<digest>` (qua đường CAS spec version, `261017-spec-version`). Từ đó mọi redeploy, reschedule, standby HA tạo mới đều chạy đúng binary đó. Digest chỉ đổi khi người dùng bấm cập nhật minor. Việc này thay cho cảnh báo "digest lệch" (không còn lệch nếu cùng pin).
- Instance không báo digest (node cũ) → `Spec.Image` giữ tag, UI hiện "chưa pin".

**Hiển thị**: `GET /databases/{id}` trả `server_version` (từ `serverVersion()`, cache 10 phút) và `image_digest`.

**Cập nhật minor** (`POST /databases/{id}/update-minor`)
1. Quyền: dedicated → cùng quyền với `upgrade` hiện có (`projectFor(..., "admin")`). **Shared → chỉ administrator của nền tảng** (cùng quyền với `/admin/settings`); không cho admin của một team, vì nó restart server của mọi tenant. Ghi audit (`database.update_minor`). Body `{"confirm_shared": true}` chỉ là xác nhận UI, không phải kiểm soát quyền.
2. `Options.HA` → 409 (chưa hỗ trợ; xem "Nợ").
3. Backup trước (`backupDatabase`); ghim backup này khỏi `OldBackups` (xem Phase 4, cột `pinned`).
4. **Pre-pull trước khi dừng**: agent pull lại tag (`RefreshImage`) và chỉ khi pull thành công mới cho rollout dừng-rồi-start. Pull lỗi → huỷ, DB giữ nguyên. Ghi rõ trong UI: có downtime vì rollout stateful là dừng-rồi-start (đo và ghi vào báo cáo).
5. Sau khi running: xoá `versionCache`, ghi digest mới vào `Spec.Image`.
6. Shared: lấy lock `shared-server-<slug>` (cùng lock `ensureSharedServer`) và từ chối khi có DB nào trên server đó đang `upgrading`.
- Chỉ tag đang chọn được làm mới (17 → 17.x mới nhất). Không bao giờ đổi major.
- Không có rollback minor: image cũ có thể bị GC; một số engine nâng data dir tại chỗ khi khởi động và không đảo ngược được. Backup ở bước 3 là điểm lùi duy nhất (restore vào DB mới cùng version).

**Xong khi**: test agent (runtime giả): `RefreshImage` pull dù image local có; pull lỗi thì không dừng instance cũ; digest báo là digest manifest nền tảng. Test server: update-minor ghi backup, đổi digest trong spec, 403 cho admin team khi shared, 403 cho developer/viewer, 409 khi HA, 409 khi có DB cùng server đang `upgrading`. Chạy tay trên máy thật: dữ liệu còn sau cập nhật; reschedule service pin digest lên node khác chạy đúng digest.

## Phase 4 — Major upgrade (một chiều)

`POST /databases/{id}/version` `{version}`. Quyền như `upgrade` hiện có (`projectFor(..., "admin")`), ghi audit `database.version`. **Không có rollback sau khi flip.**

**Phạm vi**
- Engine: postgres, mysql/mariadb, mongo. **Redis → 409** (không có `Restore`, và RDB mới không nạp lùi được).
- `version` tra cứu chính xác trong catalog; chỉ tăng theo thứ tự catalog; đích phải `ok`/`ending`.
- `Options.HA` hoặc `Options.PITR` đang bật → 409 "tắt trước, bật lại sau" (xem "Nợ").
- Trạng thái `upgrading`: mọi handler/loop khác phải từ chối hoặc bỏ qua DB đó — **bao gồm** toggle HA, enable PITR, import, resize, backup thủ công, `runScheduledBackups`, và `haTick`/failover (hiện chấp nhận `upgrading`, `dbaas_failover.go:224`: sửa lại).

**Sửa trước khi tổng quát hoá `upgradeDatabase`** (sai thứ tự hiện tại là lỗi Critical):
- `createDedicatedServer` nhận tham số để **không** gọi `SetDatabaseServer`; việc đổi con trỏ chỉ xảy ra ở bước 5.
- Slug server mới có hậu tố version (`<name>-v<major>`), tránh đụng service cũ.
- Hàm chung `moveDatabase(ctx, db, target)` dùng cho cả shared→dedicated (hành vi cũ, nay đúng thứ tự) lẫn major upgrade. Không có hàm thứ hai.
- Giữ `lock("db-"+id)` suốt job (cùng khoá HA/PITR/import/resize dùng), không chỉ trạng thái.
- Bản ghi tiến trình: cột `databases.move_state jsonb` `{phase, old_service, new_service, target_version}` ghi **trước** khi tạo server mới. `resumeProvisioning` khi thấy `move_state`: `phase < flip` → xoá server mới, khởi động lại linked service trên server cũ, `ready` kèm thông báo; `phase = flip` → hoàn tất bước 6–7. Không còn trường hợp "đặt `ready` mà không dọn".

**Các bước**
1. **Đóng băng ghi**: dừng hẳn các service đang link (dùng đường `stopService` hiện có; thêm helper nhỏ nếu chưa có), và `SetReadOnly` trên nguồn như lưới thứ hai. Downtime app = từ đây đến bước 6 (đã chốt: chấp nhận); UI cảnh báo rõ downtime và yêu cầu xác nhận trước khi gọi, API nhận `{"confirm_downtime": true}`, thiếu thì 400.
2. `backupDatabase`, **sau khi đóng băng**. Bản backup này `pinned=true` (cột mới `backups.pinned`, `OldBackups` bỏ qua), xác nhận size > 0. Ghim giữ đến khi xoá DB (đường xoá hiện có đã dọn backup theo `db.ID`).
3. Tạo đích:
   - shared: `ensureSharedServer(engine, newVersion)` rồi `CreateLogical` trên server đích (role + database) trước khi restore.
   - dedicated: `createDedicatedServer` với `db.Version = newVersion`, slug mới, không đổi con trỏ.
4. Restore (`databases.Restore`) rồi **phát lại user phụ** (đọc danh sách user phụ và mật khẩu từ store, gọi `CreateUser` trên đích; vai trò `readonly`/`readwrite`). Postgres: quyết định `--no-acl` trong `Restore` khi dump đi từ server khác (GRANT cho role chưa tồn tại làm `pg_restore` thoát khác 0).
5. **Verify theo engine**, không dùng so sánh size:
   - Postgres/MariaDB/MySQL: danh sách bảng khớp, `count(*)` từng bảng khớp (mọi bảng nếu ≤ 200, không thì 200 bảng lớn nhất theo thống kê), user phụ đăng nhập được.
   - Mongo: danh sách collection và `countDocuments` khớp.
   - Bất kỳ lệch → huỷ (đường "trước flip" ở trên).
6. **Flip** trong một câu lệnh: `UPDATE databases SET server_service_id=<new>, version=<v>, move_state=NULL WHERE id=$1 AND server_service_id=<old>` (CAS). Sau đó khởi động lại linked service (kiểm tra health server mới trước; lần lượt từng service, không restart cả loạt).
7. Dọn nguồn **ngay** (đường xoá hiện có, không `volume_gc` cho việc này): shared → `DropLogical` trên server cũ (kể cả xoá user phụ); dedicated → xoá service cũ cùng volume. Không rename, không giữ logical DB cũ, không giữ credential cũ.

**Lỗi trước flip** (bước 1–5): server cũ chưa bị đụng đến ngoài việc đóng băng ghi; huỷ → xoá đích, bỏ `SetReadOnly`, khởi động lại linked service, trạng thái `ready` + `error` mô tả. Test bằng cách giết restore giữa chừng và khẳng định `server_service_id` không đổi.

**Riêng từng engine**
- Postgres: `pg_dump` bản cũ → restore vào bản mới. Extension: dump nguồn dùng extension mà image đích không có → lỗi nêu rõ **trước** khi đóng băng (quét `pg_extension` ở nguồn so với `pg_available_extensions` trên đích, hoặc bỏ qua nếu đích chưa tạo; chốt khi làm).
- MariaDB/MySQL: `mariadb-dump`/`mysqldump`; dump logic nên không cần `*_upgrade`. Nguồn và đích khác họ (MariaDB↔MySQL) không thuộc phase này (không có "đổi engine").
- Mongo: `mongodump`/`mongorestore` bỏ qua FCV; kiểm chứng 6.0→7.0→8.0 thật; lưu ý `--drop` với `admin.system.users` khi restore (rủi ro đã ghi trong báo cáo).

**Xong khi**: chạy thật trên node: PG 16→17 và MariaDB 10.11→11.4 (shared **và** dedicated), Mongo 7.0→8.0 dedicated. Phải ghi liên tục trong lúc upgrade (writer trong test) và khẳng định không mất row nào sau flip (linked service dừng nên writer thấy lỗi, không ghi vào chỗ sai). `DATABASE_URL` của service link trỏ server mới, user phụ còn đăng nhập được, nguồn đã bị dọn, killed-restore và paasd-restart giữa các bước ở cả hai phía của flip đều về trạng thái nhất quán. Redis → 409. Đo và ghi downtime vào `plans/reports/`.

## Phase 5 — Tách `mysql` và `mariadb`

**Engine**
- Thêm hằng `MariaDB = "mariadb"`. `MySQL` giữ tên `"mysql"` nhưng chạy image `docker.io/library/mysql:8.4`; `Versions: 8.4`, default `8.4`.
- `MariaDB` mang cấu hình MariaDB hiện tại (`11.8`, `11.4`, `10.11`, `10.6`).
- `MinMemoryMB` riêng theo engine: MySQL 8.4 ≈ 512 (192 hiện tại của MariaDB quá thấp cho 8.4 khi init). Thử nano/micro trên node thật; nếu OOM, nâng sàn.
- Tham số hoá bằng một struct cho họ MySQL: `{client, dump, server, envPrefix}`. Mọi hàm trong `engines.go` hard-code `mariadb` đọc từ struct này, **không** copy từng case.
- Khác biệt thật phải xử lý riêng, có test: `MYSQL_ROOT_PASSWORD` vs `MARIADB_ROOT_PASSWORD`; `CREATE USER`/`GRANT`; `SetReadOnly` (REVOKE); `SELECT VERSION()`; `--performance-schema=OFF` (kiểm tra rồi chốt, mặc định bỏ cờ cho MySQL); `SetPassword` đã dùng `ALTER USER` (không còn là khác biệt).
- `ImportFrom`: chọn công cụ dump theo **nguồn**, không theo engine đích (MySQL 8.4 image chỉ có `mysqldump`; nguồn MariaDB cần cờ tương thích hoặc từ chối rõ ràng); bỏ `--skip-ssl-verify-server-cert` đặc thù MariaDB khi nguồn là MySQL.
- `caching_sha2_password` mặc định của 8.4: client không TLS (ví dụ PHP mysqlnd cũ) có thể không kết nối được qua `mysql://…` thường. Không khuyên và không bật `mysql_native_password` (đã tắt mặc định ở 8.4); khuyên cập nhật driver; ghi vào docs. Cân nhắc thêm tham số TLS vào `databases.URL` chỉ nếu test client thật cho thấy cần.

**Migration `split_mariadb`** (một transaction, chạy lúc khởi động; paasd là một binary do installer restart nên không có phiên bản cũ chạy song song — ghi điều này vào phần cài đặt/upgrade, và **không** làm rolling với binary cũ)
- `UPDATE databases SET engine='mariadb' WHERE engine='mysql'`.
- Service shared server: đổi `slug` `shared-mysql*` → `shared-mariadb*`, tên hiển thị, và `Spec.Language` `mysql` → `mariadb` (JSON, kèm tăng `spec_version`, **không** ép restart). Nếu `Spec.Language` bị đọc ở `deploy.go:287,351` để chọn tuning, kiểm tra tuning cho `mariadb` không đổi.
- Setting `shared_mysql_root` → copy sang `shared_mariadb_root` rồi xoá key cũ.
- Test migration: DB `mysql` cũ trên shared server → sau migration, `ensureSharedServer("mariadb", v)`, `dbCreds`, `Backup`, `Restore`, `SetReadOnly` chạy đúng trên **server cũ** (không tạo server thứ hai, không lệch mật khẩu root); `ensureSharedServer("mysql","8.4")` tạo `shared-mysql` mới, không trỏ vào container MariaDB.
- Nâng `options_version` nếu sửa `options`, như migration 017.

**Chỗ phải đổi**: danh sách ở "Sự thật đã xác minh". `prisma.go`: provider `mysql` ánh xạ engine `mysql`. `templates/compose.go:60,689`: `"mariadb": "mariadb"`, `"mysql": "mysql"`. Template WordPress chọn `mariadb` (đúng với image WordPress thường dùng). Proxy mode `plain` cho cả hai engine (public vẫn bị chặn nhờ `tcpMode` mặc định `plain`); thêm test duyệt cả hai engine. UI, CLI, `docs/API.md`.

**Xong khi**: `engine=mysql` → `SELECT VERSION()` = `8.4.x`, chạy được ở preset micro; `engine=mariadb` như trước; tạo user, quota (read-only), backup/restore, import, studio đều chạy cho cả hai; **một client thật** (Node hoặc Go driver; ghi lại PHP mysqlnd chạy hay không) kết nối `mysql://` thường; template WordPress deploy được; migration test ở trên xanh.

## Test chung và kiểm chứng

Sau mỗi phase: `go build ./... && go vet ./... && go test ./...`; web `npm run build` (nếu đổi UI). Test cần Postgres thật skip khi không có `PAAS_TEST_DATABASE_URL`: nói rõ đường nào chưa chạy (bài học từ commit 3584d36). Phase 1, 3, 4, 5 cần chạy tay trên node thật; kết quả vào `plans/reports/`.

## Nợ có chủ ý (nêu trong báo cáo, kèm điều kiện xoá)

- Major upgrade và update-minor từ chối DB `HA` (và major upgrade từ chối `PITR`). Xoá khi có người cần nâng DB HA mà không tắt HA.
- Major upgrade một chiều, không rollback. Xoá (thêm rollback) khi có người mất dữ liệu vì upgrade hỏng mà backup pre-upgrade không đủ.
- Không rollback cho cập nhật minor (backup là điểm lùi duy nhất).
- Ngày EOL nằm trong Go. Xoá/đổi khi số engine hoặc tần suất release làm việc sửa tay thành gánh nặng.
- Dump do tenant tạo được replay bằng superuser/root trên shared server (`Restore`, `engines.go`). **Có sẵn từ trước, không do plan này sinh ra**; major upgrade shared làm nó dễ kích hoạt hơn. Điều kiện xoá: một issue riêng (restore bằng role của tenant hoặc lọc `DEFINER`/event trigger); trước đó, cân nhắc cho major upgrade chỉ chạy dedicated.

## KHÔNG làm

- Catalog trong DB / UI quản trị version.
- Valkey, pgvector/PostGIS, biến thể image.
- Auto-update minor hoặc tự nâng major.
- Zero-downtime major upgrade (logical replication, `pg_upgrade --link`).
- Rollback sau major upgrade, endpoint `rollback-version`, giữ bản cũ N ngày, rename logical DB, `volume_gc` cho việc này (red-team: rename MySQL không tồn tại, credential cũ còn sống, `volume_gc` không khớp row service còn).
- Major upgrade cho Redis; cơ chế restore RDB.
- Giới hạn tần suất gọi endpoint DB (chưa có kịch bản cụ thể, chưa có hạ tầng).
- Guard chống gắn volume Alpine vào image Debian; mã hoá dump ở trạng thái nghỉ (chưa có production; không do plan này sinh ra).
- Migration dữ liệu alpine → Debian cho DB dev hiện có (tạo lại).
- MySQL 8.0 và 9.x (8.0 đã EOL, 9.x là innovation).
- Rolling upgrade paasd giữa binary trước và sau migration `split_mariadb`.

## Red Team Review

### Session — 2026-10-20
**Findings:** 15 (12 accepted, 3 rejected)
**Severity breakdown:** 5 Critical, 7 High, 3 Medium

| # | Finding | Severity | Disposition | Applied To |
|---|---------|----------|-------------|------------|
| 1 | Con trỏ đổi trước restore; resume không dọn trạng thái dở | Critical | Accept | Phase 4 (sửa trước, `move_state`, CAS, lock) |
| 2 | Không đóng băng ghi trong dump/restore | Critical | Accept | Phase 4 bước 1–2 |
| 3 | Dedicated→dedicated đụng slug | Critical | Accept | Phase 4 (slug có hậu tố version) |
| 4 | Migration mysql→mariadb mồ côi shared server và root password | Critical | Accept | Phase 5 migration + test |
| 5 | Redis không có `Restore` | Critical | Accept | Phase 4 phạm vi (409) |
| 6 | Giữ bản cũ N ngày: rename/credential/`volume_gc`/xoá DB | High | Accept (chọn một chiều, user quyết) | Phase 4 bước 7, KHÔNG làm, Nợ |
| 7 | Restore thiếu user phụ/`CreateLogical`; verify bằng size yếu | High | Accept | Phase 4 bước 3–5 |
| 8 | Update-minor dừng trước pull, không rollback, digest index | High | Accept | Phase 3 (pre-pull, digest nền tảng, Nợ) |
| 9 | Update-minor shared: admin team restart server mọi tenant | High | Accept (sửa: platform admin) | Phase 3 bước 1, 6 |
| 10 | "Pin digest" chỉ quan sát, `Spec.Image` vẫn là tag | High | Accept (sửa: ghi `@sha256` vào spec) | Phase 3 "Pin thật" |
| 11 | MySQL 8.4: RAM sàn, `caching_sha2_password`, dump tool import | High | Accept | Phase 5 |
| 12 | Backup retention xoá bản pre-upgrade; handler khác chạy khi `upgrading` | High | Accept | Phase 4 (`pinned`, chặn handler) |
| 13 | Backfill đoán version; `eol_notified` trong `options`; số migration cứng | Medium | Accept | Phase 2, đầu plan |
| 14 | Dump tenant replay bằng superuser trên shared server | Medium | Reject (có sẵn từ trước, ghi vào Nợ) | — |
| 15 | Public MySQL port lộ sau khi đổi nhãn | Medium | Reject phần lớn (`tcpMode` mặc định `plain`); thêm test hai engine; bỏ guard Alpine, mã hoá dump, rate limit | Phase 5 test, KHÔNG làm |

## Validation Log

### Session 1 — 2026-10-20
**Câu hỏi:** 3. Các quyết định:

| Chủ đề | Quyết định | Áp vào |
|---|---|---|
| Thứ tự phase | 1 → 2 → 5 → 3 → 4 (tách MySQL/MariaDB trước pin digest và major upgrade) | Đầu plan |
| Môi trường test | Có cluster dev 2 node; HA, switchover, reschedule digest chạy được | Đầu plan, Phase 1/3/4/5 "Xong khi" |
| Downtime major upgrade | Dừng hẳn service link; UI cảnh báo, API cần `confirm_downtime` | Phase 4 bước 1 |

Không có câu hỏi nào khác: các quyết định còn lại (một chiều, Redis loại, HA/PITR bị từ chối, platform admin cho shared, EOL theo catalog) đã chốt ở brainstorm và red-team.
