# Triển khai 261020-db-versions — 2026-10-20

Plan: `plans/261020-db-versions/plan.md`. Thứ tự đã làm: 1 → 2 → 5 → 3 → 4, mỗi phase một commit.

| Phase | Commit | Nội dung |
|---|---|---|
| 1 | 789b9aa | Postgres chạy image Debian (`postgres:<v>`), bỏ `-alpine` |
| 2 | 643fdfd | Catalog `[]Version{Name,EOL}`, `Status`/`CheckCreate`, 409 khi EOL, `warning` khi `ending`, banner/badge, notify `database.eol` (cột `eol_notified`), migration 018 |
| 5 | 1d25e6d | Tách `mysql` (8.4) và `mariadb`; struct `myFamily`; migration 019 `split_mariadb` |
| 3 | e2e0b6e | Pin digest vào `Spec.Image` + deployment, `CmdRefreshImage`, `POST /databases/{id}/update-minor`, `backups.pinned`, migration 020 |
| 4 | dda1bb5 | `POST /databases/{id}/version` (một chiều), `moveDatabase` dùng chung với shared→dedicated, `move_state`, verify theo engine, chặn handler khi `upgrading`, migration 021 |

## Đã kiểm chứng
- `go build ./... && go vet ./... && go test ./...` xanh, **kể cả test cần Postgres thật** (Postgres 17 trong podman, `PAAS_TEST_DATABASE_URL`): migration 018–021, test shared/dedicated success, lỗi trước flip (5 ca), resume hai phía flip, mọi handler bị chặn khi `upgrading`.
- Chạy thật trong container tạm: lệnh dump/restore/verify của PG 16→17 (dedicated + shared), MariaDB 10.11→11.4 (dedicated + shared), MySQL 8.4, Mongo 7→8.0; SQL của MySQL 8.4.11 (CREATE USER/GRANT/REVOKE/ALTER USER/VERSION).
- Phát hiện và sửa khi chạy thật: `CheckLogin` Postgres qua 127.0.0.1 bị pg_hba trust nên nhận mọi mật khẩu (nay qua `$(hostname)`); `VersionCommand` của Mongo không kết nối nên không dùng làm probe sẵn sàng (nay dùng `databases.Stats`).

## CHƯA chạy (cần node thật, theo "Xong khi" của plan)
- Phase 1: PG 17/18 dedicated, HA + switchover, PITR, import từ PG ngoài, PgBouncer, `PGDATA` PG 18 còn sau khi tạo lại container.
- Phase 2: PG 14 → 201 + warning qua HTTP (chỉ có unit test `CheckCreate`).
- Phase 5: MySQL 8.4 ở preset micro (sàn 512 MB, có OOM không), client thật qua `mysql://` (Node/Go; PHP mysqlnd chạy hay không), WordPress template, import hai chiều MySQL↔MariaDB.
- Phase 3: dữ liệu còn sau update-minor, reschedule service đã pin sang node khác chạy đúng digest, pull bằng `@sha256:<digest manifest nền tảng>` trên node thứ hai, đo downtime, agent cũ trả "too old".
- Phase 4: PG 16→17 và MariaDB 10.11→11.4 (shared + dedicated), Mongo 7→8 dedicated; writer ghi liên tục trong lúc upgrade; kill paasd hai phía flip; service link nối lại `DATABASE_URL` mới; đo downtime.

## Quyết định lệch plan (có lý do)
- Luật EOL đặt ở `databases.CheckCreate` (gọi từ `createDatabase` và PITR restore), không đặt trong `ServerSpec`: `ServerSpec` còn dựng lại spec cho DB đang chạy (resize, HA toggle), gate ở đó sẽ làm DB đang chạy hỏng sau ngày EOL. Hệ quả: PITR restore từ DB đã EOL trả 409.
- Tag không có trong catalog trong compose/template (vd `mariadb:10.6`, `mysql:8.0`) lặng lẽ rơi về version mặc định thay vì báo lỗi.
- `ImportFrom`: công cụ dump theo engine đích (URL `mysql://` không cho biết nguồn là họ nào); tương thích bằng probe cờ.
- Flip CAS ghi `phase=flip` thay vì `move_state=NULL`; `move_state` xoá ở cuối bước 7 (nếu không, crash giữa flip và dọn thì không còn gì để resume). `move_state` có thêm `target_dedicated`, `old_shared`, `stopped[]`.
- `update-minor` shared bắt buộc `confirm_shared` (400 nếu thiếu); `version` trả 409 khi bật pooler (pgbouncer có `DB_HOST` cố định).
- Mongo restore `foreign`: `--nsExclude=admin.* --nsExclude=config.*`, không `--drop`.

## Nợ có chủ ý (còn hiệu lực, kèm điều kiện xoá — xem plan)
- Major upgrade/update-minor từ chối DB HA (major từ chối cả PITR) — xoá khi có người cần.
- Major upgrade một chiều, không rollback minor — xoá khi có người mất dữ liệu mà backup pre-upgrade không đủ.
- Ngày EOL trong Go.
- Dump tenant replay bằng superuser trên shared (có sẵn từ trước).
- Mới phát hiện, chưa có điều kiện xoá nào đã được chốt:
  - `update-minor` job không resume được sau khi paasd restart (`resumeProvisioning` đặt `upgrading` về `ready`, không rollback).
  - `update-minor` trên server shared đích không thấy DB đang move (nó nằm trên server cũ) nên có thể restart đích giữa lúc restore (cửa sổ nhỏ).
  - Shared→dedicated `upgrade` vẫn làm hỏng pooler (có sẵn từ trước).
  - Container chạy trước khi nâng agent chỉ được pin ở lần start kế tiếp.
  - `pinFirstDigest` bump `services.version` → form settings đang mở gặp một lần 412.
  - Postgres `--no-acl` bỏ các GRANT thủ công cho role ngoài user phụ của DB (đã ghi trong `docs/API.md`).
  - `templateDBValues` dựng host shared là `"shared-"+engine`, sai với version không mặc định (có sẵn từ trước).

- Từ review sau cùng (đã sửa: gate rotate/delete user, public, quota, delete DB khi `upgrading`; `ready` khi lỗi sớm; flip mơ hồ không abort). Còn lại:
  - Backup pinned của lần upgrade bị abort tồn tại đến khi xoá DB (retry nhiều lần sẽ tích dump). Xoá khi có DB lớn bị retry nhiều.
  - `freezeWrites` chỉ dừng service `Running`; service đang starting vẫn ghi (verify bắt insert, không bắt update tại chỗ).
  - Link/unlink service trong lúc move chưa bị chặn.
  - Sau flip mơ hồ, DB giữ `upgrading` đến lần restart paasd kế tiếp (resumeMove chỉ chạy lúc khởi động).
  - Lỗi 3/4 của review chưa có test (cần inject lỗi store).

## Triển khai
paasd là một binary do installer restart: không chạy song song với binary cũ khi qua migration `split_mariadb` (019). DB dev dùng alpine phải tạo lại.
