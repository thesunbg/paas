---
title: DB HA không split-brain (lease + self-fence + nhân chứng), promote idempotent, nhận lại instance stateful, PITR ra S3
status: pending
created: 2026-10-17
depends_on: 261017-leader-fencing
---

# E — DB HA và độ bền dữ liệu

Lý do, sự thật đã xác minh và phương án đã loại: `plans/reports/brainstorm-261017-db-ha.md`.

## Hằng số (một chỗ, có comment chứng minh thứ tự)

Đặt trong `internal/proto` để agent và CP dùng chung:
- `LeaseTTL = 20s`: agent coi lease hết hạn sau từng này kể từ lần gửi heartbeat cuối cùng được ack.
- `FenceCheckEvery = 5s`
- `WalSenderTimeout = 10s`: đặt luôn thành `wal_sender_timeout` và `wal_receiver_timeout` của server.

Trong `server`:
- `haPrimaryGone = 45s`, phải ≥ `LeaseTTL` + 25s.
- `haWitnessDown = 30s`, phải ≥ `WalSenderTimeout` + `FenceCheckEvery` + biên.
- `haLeaderGrace = 60s`
- `haMaxLagBytes = 16 MiB`

Comment phải ghi chuỗi suy luận sau. Tại thời điểm promote T: lần gửi được ack cuối cùng của P ≤ `last_seen_at` < T−45, nên lease của P đã hết từ trước T−25. Standby không stream từ T−30, nên walsender của P đã thoát trước T−20 và agent của P đã fence trước T−15.

## Phase 1 — Lease và self-fence (proto, agent, server)

**Proto**
- `HeartbeatAck{Unfence []string}`.
- `ContainerStatus.Fenced bool`.
- `RunRequest.HAPostgres bool` (agent lưu thành label của container).

**Server**
- `onHeartbeat` reply `HeartbeatAck` **chỉ sau khi** `NodeHeartbeat` thành công. `NodeHeartbeat` lỗi thì không reply.
- `Unfence` gồm các container có `Fenced=true`, vẫn còn row instance, và DB của chúng không có `db_ha_state.failover_to`.
- `dedicatedSpec` và `createReplica` set `HAPostgres=true` khi `db.Options.HA`, thêm `-c wal_sender_timeout=10s -c wal_receiver_timeout=10s`.

**Agent**
- `heartbeat` đổi từ `bus.Publish` sang `bus.Request` (timeout 5s). Ghi lại `sentAt` (monotonic) trước khi gửi. Ack OK thì `leaseUntil = sentAt + LeaseTTL` và `leaseSeen = true`.
- Áp các container trong `ack.Unfence`: start lại container đã fence, rồi xoá cờ fenced.
- Thêm vòng `fenceLoop` (5s). Cờ `leaseSeen=false` thì không fence (CP cũ chưa ack, tránh fence hàng loạt khi rolling upgrade). Ngược lại, khi `now > leaseUntil` thì với mỗi container `HAPostgres` đang chạy:
  - exec `sh -c 'PGPASSWORD=$POSTGRES_PASSWORD psql -h 127.0.0.1 -U $POSTGRES_USER -d postgres -Atc "SELECT pg_is_in_recovery(), (SELECT count(*) FROM pg_stat_replication WHERE state=''streaming'')"'` với timeout 3s;
  - gọi `agentShouldFence(execErr, inRecovery, streaming)`, là hàm thuần: exec lỗi → true; đang recovery → false; streaming=0 → true;
  - fence = dừng container (grace 2s) rồi đánh `fenced`. Log ở mức Warn.
- Cờ fenced phải sống qua restart agent (lưu bên cạnh state container) và được báo lên trong `ContainerStatus`.
- **Kiểm chứng trước khi viết**: biến `POSTGRES_USER` và `POSTGRES_PASSWORD` có trong env khi exec vào container. Nếu không có thì truyền tên user qua label lúc run, còn password đọc từ env của container.

## Phase 2 — Quyết định failover (server)

Migration `015_db_ha.sql`:
- `db_ha_state(database_id PK FK, primary_lsn pg_lsn, lsn_at timestamptz, failover_to text, updated_at)`
- `volume_gc(node_id, service_id, delete_after timestamptz, PRIMARY KEY(node_id, service_id))`

**Leader**: `leaderLoop` ghi lại `s.leaderSince` khi lên làm leader.

**Hàm thuần `failoverVerdict(in haInput) (verdict, reason string)`**

`verdict` ∈ {`wait`, `promote`, `blocked_lag`}.

Input:
- `sinceLeader`
- `nodesTotal`, `nodesOffline`
- `primaryLastSeenAge`
- `witness{ok bool, streaming bool, downFor time.Duration}`
- `standbyLSN`, `primaryLSN`, `force bool`

Thứ tự kiểm tra:
1. Grace sau khi lên leader.
2. `nodesOffline*2 > nodesTotal`.
3. `primaryLastSeenAge ≤ 45s`.
4. Nhân chứng lỗi hoặc còn streaming.
5. `downFor < 30s`.
6. `primaryLSN − standbyLSN > 16 MiB` và không có `force` → `blocked_lag`.

**`haTick`**
- Primary online: exec `SELECT pg_current_wal_lsn()` trên primary, upsert vào `db_ha_state` (fenced theo epoch).
- Primary có vẻ đã chết: exec trên standby `SELECT status, extract(epoch FROM now()-last_msg_receipt_time), pg_last_wal_receive_lsn() FROM pg_stat_wal_receiver` (không có row nghĩa là không stream). Giữ `witnessDownSince[dbID]` trong bộ nhớ. Mất khi đổi leader thì cũng không sao, vì grace 60s > 30s.
- Gọi `failoverVerdict`.
- Kết quả `blocked_lag` thì notify `database.failover` (một lần cho mỗi sự cố) với nội dung "standby lag X MB, cần force".
- Ở mức Info, log lý do `wait` mỗi khi lý do đổi, để debug.
- `POST /api/v1/databases/{id}/ha/failover {force:true}` (admin) cho phép bỏ qua bước 6. Các bước 1–5 **vẫn áp dụng**: force không bao giờ được phá điều kiện fence.

## Phase 3 — Promote idempotent (server, store)

**Store**
- `SetFailoverIntent(ctx, dbID, repID)` / `FailoverIntent(ctx, dbID)`.
- `CommitFailover(ctx, dbID, newServerID, oldServerID, aliases, gcNode)`. Làm trong một tx (fenced):
  - chuyển `Spec.DNSAliases` và `HealthCheck.TimeoutSec` sang service mới (qua đường CAS spec version, D2);
  - `databases.server_service_id`;
  - `options.ReplicaServiceID=""`;
  - xoá các row instance của service cũ, đánh service cũ là đã xoá (theo đúng cách `deleteService` đang làm ở tầng store);
  - insert `volume_gc(gcNode, old, now()+7d)`;
  - xoá `failover_to`.

**`promoteStandby`** viết lại thành các bước, mỗi bước chạy lại được:
1. `planned` → `stopService(old)` như cũ. Unplanned → gửi `CmdStop` tới node cũ best-effort, timeout 5s, bỏ qua lỗi.
2. `SetFailoverIntent`.
3. `SELECT pg_is_in_recovery()` trên standby: `t` → `pg_promote(true, 60)`; `f` → bỏ qua bước này.
4. `CommitFailover`.
5. Bên ngoài tx: `publishAll`, notify, base backup PITR (Phase 5).

**Phần còn lại**
- `haTick`: `failover_to` còn đó và đang làm leader thì chạy `promoteStandby` từ bước 3 (bỏ qua verdict, vì quyết định đã được đưa ra rồi).
- Maintenance loop: `volume_gc` nào quá hạn và node đang online thì `removeVolumes`, rồi xoá row. Xoá `removeVolumesWhenOnline`.
- Switchover theo kế hoạch dùng chung đường này.

## Phase 4 — Nhận lại instance stateful (store, server)

- `reconcileLocked`, case `InstLost` mà node không online: service có `len(Spec.Volumes)>0` hoặc `NodeID != nil` thì **giữ row** và tính nó vào `healthy`, để không gọi `startInstance` cho node đã pin. Service stateless giữ nguyên hành vi cũ.
- `ApplyContainerStatus`: `status IN ('running','failed','lost')`.
- Trường hợp node về mà không báo container (máy reboot) thì đi theo nhánh cũ `InstLost` + node online → stop + start mới. Không đổi.
- Kiểm tra lại: `NodeCommitted` không tính `lost`, nên khi node về mà container sống lại thì nó lại được tính. Ghi nhận việc này trong test.

## Phase 5 — PITR ra S3 (s3, agent, server)

**`internal/s3`**
- `List(prefix) ([]Object{Key, Size, LastModified}, error)`, dùng ListObjectsV2 và phân trang.
- `CreateMultipart(key) (uploadID)`.
- `PresignPart(key, uploadID, n, expires)`.
- `CompleteMultipart(key, uploadID, parts []{N, ETag})`.
- `AbortMultipart`.
- Test với fake HTTP server, theo `s3_test.go`.

**Agent**
- `TransferRequest` thêm `PartURLs []string` và `PartSize int64`. Có parts thì upload từng phần.
- `GenericReply` (hoặc reply mới) trả `ETags []string`.

**Server**
- `handleSetPITR`: `!s.s3.Enabled()` → 400 "point-in-time recovery cần object storage (S3)".
- `dedicatedSpec`:
  - `archive_command=test ! -f /wal/archive/%f && cp %p /wal/archive/.%f.tmp && mv /wal/archive/.%f.tmp /wal/archive/%f`;
  - standby của DB có PITR chạy `archive_mode=always`.
- `walShipTick` (leader, mỗi 30s, `tryLock("pitr-ship-"+db)`). Với primary và standby của mỗi DB có PITR:
  - exec `ls -1 /wal/archive` (bỏ qua `.tmp`);
  - presign PUT `pitr/<db>/wal/<file>`, `CmdTransfer`, thành công thì `RemoveFiles`;
  - file cũ nhất trong spool đã quá 10 phút thì set status message cảnh báo.
- `takeBaseBackup`:
  - ghi `backups/<db>/base-*.tar.gz` như cũ;
  - `size > 64 MiB` → multipart (part 64 MiB), không thì một PUT;
  - thành công thì `SetBackupS3Key` và xoá file local. `Backup.NodeID` vẫn ghi để biết nơi sinh ra.
- `prunePITR`: xoá base trên S3 vượt retention (luôn giữ bản mới nhất), và xoá WAL trên S3 có `LastModified` < base cũ nhất đang giữ − 30 phút. Giữ mọi `.history`.
- `restorePITR`:
  - chọn node bằng `pickReplicaNode`, bỏ ràng buộc "cùng node";
  - tải base và mọi WAL có `LastModified ≥ base.CreatedAt − 30m`, cùng mọi `.history`, về `volumes/<new>/restore-wal/`;
  - `PITRPrepare{WALSource: đó}`;
  - xong thì `RemoveFiles` thư mục tạm.
- `promoteStandby` sau failover: vẫn chụp base backup mới, nhưng không còn bắt buộc cho tính liên tục.
- DB đang bật PITR mà cluster không có S3: `pitrTick` bỏ qua, set status "PITR paused: cần S3". Không xoá gì.

## Phase 6 — Test

**Unit (thuần)**
- `failoverVerdict`, dạng bảng, phải có các dòng:
  - trong grace → wait;
  - 2/3 node offline → wait;
  - 1/2 node offline → được phép;
  - last_seen 40s → wait;
  - nhân chứng lỗi exec → wait;
  - nhân chứng streaming → wait;
  - down 20s → wait;
  - lag 20 MiB → blocked_lag;
  - lag 20 MiB + force → promote;
  - force mà vẫn đang trong grace → wait;
  - đủ điều kiện → promote.
- `agentShouldFence`: exec lỗi, đang recovery, streaming>0, streaming=0.
- Kiểm tra hằng số: `haPrimaryGone ≥ LeaseTTL+25s` và `haWitnessDown ≥ WalSenderTimeout+FenceCheckEvery+10s`, để sửa sai một hằng số là test đỏ.

**Agent** (runtime giả, đồng hồ inject được)
- Không ack 20s mà psql báo primary và 0 standby → container bị dừng và `Fenced=true`.
- Còn standby streaming → không fence.
- `leaseSeen=false` → không fence.
- Nhận `Unfence` → start lại.

**Store / server (Postgres thật, skip khi không có `PAAS_TEST_DATABASE_URL`)**
- `CommitFailover` là atomic: chạy với epoch cũ thì `ErrFenced` và không có gì đổi.
- Promote resume: tạo `failover_to` và một standby giả đã `in_recovery=f` (fake exec) → `haTick` hoàn tất, không gọi `pg_promote`, chỉ notify một lần.
- Reconcile: instance `lost` của service có volume trên node offline thì giữ row và không gọi `CmdRun`. Heartbeat báo container đang chạy → `running`.
- `volume_gc` chỉ xoá khi đã quá hạn và node online.
- Ack heartbeat: `NodeHeartbeat` lỗi thì không reply.
- `s3`: list có phân trang, multipart complete với ETag đúng thứ tự.

**E2E 2 máy** (script trong `scripts/e2e/`, ghi kết quả vào `plans/reports/`). Sampler 1s chạy trên **mỗi máy, cục bộ**, ghi `pg_is_in_recovery()` của container postgres trên máy đó.
1. CP↔P bị cắt: P ở máy B, CP ở máy A. Trên B dùng nftables drop cổng NATS tới A, WireGuard vẫn để mở, trong 3 phút. Kỳ vọng: không failover, P không bị fence (standby vẫn stream), app trên A vẫn ghi được.
2. P bị cô lập hoàn toàn: drop mọi thứ giữa A và B, trong 3 phút. Kỳ vọng: P bị fence ở khoảng t≈20–35s, S được promote ở t≥45s, và **không giây nào có hai `f`**. Đây là assert của tiêu chí xong.
3. Phục hồi: gỡ rule. Kỳ vọng: container P cũ bị dừng/xoá, có row `volume_gc`, standby mới được tạo trên B.
4. Kill leader (SIGKILL `paasd`) ngay sau log "failing over database" → CP chạy lại, failover hoàn tất, chỉ có một notification.
5. DB không HA có volume trên B, cắt B 2 phút rồi gỡ → cùng container ID, `restarts` không đổi.
6. PITR: ghi dữ liệu, failover, ghi tiếp, restore tới thời điểm sau failover lên node A → dữ liệu khớp. `ls volumes/*/wal/archive` trên cả hai node rỗng sau 1 phút.

Cuối cùng: `go build ./... && go vet ./... && go test ./...`.

## KHÔNG làm

- Sync replication (chỉ làm khi có tenant cần RPO=0, khi đó làm thành option theo DB).
- Failover khi postgres chết mà node vẫn online.
- Watchdog systemd cho agent (đây là lỗ còn lại khi agent chết đồng thời với P↔S bị cắt).
- Fencing ở agent theo epoch leader. Promote idempotent đã đủ.
- Nhiều standby, chọn standby tốt nhất theo LSN (hiện chỉ có một standby).
- `archive_command` gọi thẳng S3 hoặc gọi qua socket của agent.
- PITR fallback local.
- Đổi heartbeat của node không có container HA. Ack áp cho mọi node, nhưng fence chỉ đụng tới container `HAPostgres`.
- Bảng hoặc UI mới để xem lease. Lý do `wait` chỉ có trong log, cộng với notify cho `blocked_lag`.

## Thứ tự và phụ thuộc

Phase 1 → 2 → 3 phải đi theo thứ tự. Riêng 2 và 3 có chung migration. Phase 4 và 5 độc lập, chạy song song được với 1–3. Tiêu chí split-brain chỉ được đánh giá sau khi xong 1+2+3 (E2E bước 1–4).
