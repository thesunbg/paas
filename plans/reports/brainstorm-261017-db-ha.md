# Brainstorm: E — DB HA và độ bền dữ liệu

## Vấn đề

- `haTick` (`dbaas_ha.go:293`) failover chỉ dựa vào một tín hiệu: `nodes.status='offline'` cộng 30 giây. Tín hiệu này chỉ đo đường heartbeat NATS giữa agent và control plane (CP). Mạng CP↔P bị cắt trong khi primary vẫn sống và standby vẫn stream là failover vẫn chạy, và ta có **hai primary**.
- **Hiện không có fence nào.** "Xoá service primary cũ" chỉ dừng container *khi node quay lại* (`reconcile.go:44`). Trong lúc node bị cô lập, agent trên đó vẫn giữ bảng DNS cũ, nên app cùng node vẫn ghi vào primary cũ, vô thời hạn.
- Heartbeat là `Publish` một chiều (`agent.go:244`). Agent không biết CP có nghe thấy mình không, nên không thể tự fence.
- Promote không idempotent. `pg_promote` thành công nhưng leader chết trước `SetDatabaseServer` thì leader mới gọi lại `pg_promote` và nhận lỗi "not in recovery", DB kẹt. D3 cũng đã ghi nhận `pg_promote` có thể lọt qua cửa sổ check fence.
- Không có LSN check. Standby lag bao nhiêu cũng promote.
- Node offline thì instance thành `lost`, rồi reconcile xoá row (`reconcile.go:202`). Khi node về, heartbeat thấy container không có row và **dừng nó**. Với service có volume hoặc bị pin, đó là một lần restart vô ích (với DB là mất kết nối), và trong lúc node vắng mặt thì reconcile spam `startInstance` thất bại.
- `removeVolumesWhenOnline` là goroutine, đổi leader là mất. Nó cũng xoá data của primary cũ, trong khi data đó có thể chứa write chưa kịp replicate.
- PITR: WAL archive và base backup nằm trên **cùng node** với dữ liệu. Mất node là mất cả hai. Sau failover chuỗi restore bị gãy (`restorePITR` từ chối khi khác node). `handleTransfer` dùng một PUT duy nhất, nên file trên 5 GB fail với S3. `archive_command` dùng `cp` không atomic, crash giữa chừng là segment hỏng kẹt vĩnh viễn (`test ! -f`).

## Sự thật đã xác minh

- CP không với tới được node bị cô lập. Vì vậy **không cơ chế nào chỉ chạy ở CP đáp ứng được tiêu chí "không có hai primary"**. Fence bắt buộc phải nằm ở phía node.
- DNS nội bộ là của từng agent, do CP đẩy qua NATS (`agent.go:162`). Node bị cô lập giữ alias trỏ về primary cũ.
- Heartbeat 5s (`agent.go:175`). `MarkStaleNodesOffline` 20s. Heartbeat đi qua `QueueSubscribe`, nên một CP trả lời là đủ cho request/reply.
- `ApplyContainerStatus` chỉ chuyển trạng thái từ `running|failed`. Row `lost` không bao giờ tự sống lại.
- Scheduler: `PinnedNode` mà node không online thì trả `ErrNoCapacity`.
- Không có `CmdStart`. Unfence phải đi đường khác (xem quyết định 4).
- S3 hiện có `Presign`, `Delete`, `Key`. Chưa có List và multipart.

## Quyết định (đã chốt với user)

| # | Quyết định | Lý do |
|---|---|---|
| 1 | **Lease ở agent.** Heartbeat đổi thành request/reply. CP chỉ ack sau khi `NodeHeartbeat` commit. Agent tính `leaseUntil = t_gửi + L` (L=20s, đồng hồ monotonic). | Một lần gửi được ack nghĩa là `last_seen_at ≥ t_gửi`. Agent luôn thấy lease hết hạn **trước** khi CP thấy. Hai bên không phải so đồng hồ với nhau. |
| 2 | **Agent tự fence** container gắn nhãn `ha-postgres` khi: lease hết hạn VÀ `pg_is_in_recovery()=f` VÀ không còn replica `streaming` trong `pg_stat_replication`. Exec lỗi hoặc timeout cũng fence. Fence = dừng container. | Kiểu failsafe của Patroni. CP sập toàn bộ mà standby vẫn stream thì DB vẫn chạy, nên sự cố control plane không biến thành sự cố data plane. Xét role lúc chạy nên container standby đã được promote cũng tự được bảo vệ. |
| 3 | **Standby làm nhân chứng độc lập.** CP hỏi agent của node standby (một đường khác hẳn CP↔P): `pg_stat_wal_receiver` còn `streaming` và nhận message trong 10s gần nhất → **huỷ failover**. Chỉ promote khi standby báo không stream liên tục ≥ 30s. Exec trên standby lỗi thì không failover. | Phân biệt được "CP mất P" với "P chết". Khoảng 30s ≥ `wal_sender_timeout` (10s) + chu kỳ check fence của agent (5s) + biên. Nghĩa là trước khi promote, P chắc chắn đã thấy mất standby và đã fence. |
| 4 | **Unfence qua ack.** `ContainerStatus.Fenced`. Ack trả về danh sách container đang fenced mà vẫn còn row instance và DB không có failover dở. Agent start lại chúng. | Không cần thêm command. Có row instance nghĩa là service chưa bị failover thay (failover xoá primary cũ trong cùng tx với lệnh đổi server). Ack được nghĩa là lease vừa được làm mới, nên leader sẽ không failover. |
| 5 | **Điều kiện failover** là một hàm thuần `failoverVerdict(...)`. Cả các điều kiện sau phải đúng: đã làm leader được ≥ 60s; số node offline ≤ một nửa (đếm `>`, không phải `≥`); `now() - last_seen_at > 45s`; nhân chứng không stream liên tục ≥ 30s; standby không lag vượt ngưỡng. | Gom một chỗ để unit test bằng bảng kịch bản. 45s = L(20) + 25s biên. |
| 6 | **LSN.** Mỗi tick, leader ghi `pg_current_wal_lsn()` của primary vào bảng `db_ha_state` (KHÔNG ghi vào `options`, vì options là CAS-versioned và ghi 10 giây một lần sẽ làm hỏng các lệnh sửa của user). Lúc failover, `pg_last_wal_receive_lsn()` của standby nhỏ hơn LSN ghi nhận quá **16 MB** thì không auto failover: notify admin và mở API `force` kèm cảnh báo mất dữ liệu. | Giống `maximum_lag_on_failover` của Patroni. User chọn an toàn hơn availability. |
| 7 | **Promote idempotent.** Ghi ý định `db_ha_state.failover_to` (fenced theo epoch D3), rồi `pg_is_in_recovery()`: còn `t` thì `pg_promote`, đã `f` thì bỏ qua. Sau đó **một tx**: chuyển alias, `SetDatabaseServer`, xoá `ReplicaServiceID`, xoá service cũ cùng các row instance, xoá `failover_to`. `haTick` thấy `failover_to` còn đó thì chạy tiếp từ bước kiểm tra recovery. Trước khi promote, gửi `CmdStop` best-effort tới node primary cũ. | Chạy lại ở bất kỳ bước nào cũng an toàn. Không còn trạng thái nửa vời giữa các lệnh ghi store. |
| 8 | **Giữ volume primary cũ 7 ngày.** Bảng `volume_gc(node_id, service_id, delete_after)`. Maintenance loop xoá khi node online và đã quá hạn. Bỏ `removeVolumesWhenOnline`. | Cứu được tay phần write chưa replicate. Bền qua lần đổi leader. |
| 9 | **Nhận lại instance stateful.** Service có volume hoặc `NodeID` mà node offline thì giữ row `lost`, không xoá, không thay. `ApplyContainerStatus` cho `lost → running|failed`. | Node về thì đúng container cũ sống tiếp, không restart. Hết spam startInstance. |
| 10 | **PITR chỉ dùng S3** (bắt buộc). Prefix theo **DB** (`pitr/<db>/wal/`, `pitr/<db>/base/`), không theo server, nên chuỗi restore không gãy qua failover (file `.history` của timeline cũng được ship). | Đúng yêu cầu "không cùng node". Một đường code. |
| 11 | **Ship WAL do CP điều khiển.** `archive_command` ghi vào spool `/wal/archive` (cp sang tmp rồi mv). Mỗi 30s leader list spool, presign PUT, agent upload, thành công thì xoá local. Standby chạy `archive_mode=always` để spool cả WAL nhận được. Hai bên cùng upload một segment thì nội dung giống hệt nhau. | Theo mẫu `transfer` đã có. Container của tenant không cầm credential S3. `always` trên standby bịt lỗ hổng: primary chết khi spool chưa kịp ship thì standby vẫn còn bản. |
| 12 | **Base backup**: ghi file tạm trên node, upload **multipart** (presign từng part), rồi xoá local. **Restore** tải base cùng WAL cần thiết từ S3 về một node bất kỳ, rồi chạy `PITRPrepare` như cũ. | Đi được qua giới hạn 5 GB. Restore không còn bị trói vào node nguồn. |

## Phương án đã loại

- **Chỉ sửa ở CP (nhân chứng, grace, quorum) mà không fence ở node.** Kịch bản P bị cô lập hoàn toàn vẫn sinh hai primary, vì app cùng node với P ghi vào P qua DNS cũ. Không đạt tiêu chí.
- **Synchronous replication.** Primary cũ không commit được, nhưng mỗi write chậm thêm một RTT. Standby chết thì write treo tới khi CP gỡ sync, mà CP lại có thể đang chính là bên bị cắt. Vẫn còn hai process primary. Điều kiện để làm: có tenant cần RPO=0. Khi đó làm thành option theo từng DB, xếp chồng lên self-fence.
- **Self-fence chỉ dựa vào lease.** CP hoặc Postgres của CP sập là mọi DB HA dừng sau 20s.
- **Promote dù lag lớn.** User chọn không mất dữ liệu ngầm.
- **Container tự gọi S3 trong `archive_command`** (wal-g hoặc curl). Phải đưa credential vào container của tenant, và S3 chung không có credential theo prefix. Còn biến thể qua unix socket tới agent thì cho ngữ nghĩa archive chặt hơn, nhưng image postgres chính thức không có curl và phải mount thêm binary. Với `archive_mode=always` trên standby thì khoảng hở của spool đã được che.
- **Fallback PITR local khi không có S3.** Phải giữ hai đường code, và vi phạm yêu cầu "không cùng node".
- **Xoá volume primary cũ ngay khi node về.** User chọn giữ 7 ngày.
- **Fencing ở agent theo epoch của leader.** D3 đã để lại cửa sổ vài ms cho `pg_promote`. Promote idempotent (quyết định 7) xử lý được, không cần đổi proto thêm.

## Rủi ro và nợ ghi nhận

- **Tính đúng đắn dựa trên giả thiết agent còn sống.** Agent crash hoặc treo trên P trong khi postgres vẫn chạy thì không có self-fence. Lúc đó heartbeat cũng tắt, nhưng nhân chứng vẫn thấy stream nên không failover. Chỉ khi agent chết *và* P↔S bị cắt cùng lúc (hai lỗi chồng nhau) thì mới có thể có hai primary. Muốn bịt lỗ này thì cần watchdog systemd cho agent. Chưa làm.
- Dựa trên tốc độ đồng hồ monotonic của hai máy (không cần đồng bộ với nhau). Biên 25s là đủ rộng.
- Fence = dừng hẳn container, nên khi lease mất mà standby cũng mất thì có downtime. Đây là chủ ý: an toàn hơn availability.
- Postgres chết mà node vẫn online (OOM, crash loop) thì **vẫn không failover**. Hành vi như hiện tại, không thuộc E.
- Cluster 2 node mà máy chứa CP chết thì không còn ai làm failover. CP không HA trong setup này.
- Spool chưa ship thì có thể mất tối đa khoảng 30s + `archive_timeout` WAL **trên S3**, khi primary chết và HA tắt. Bật HA thì standby che được.
- PITR của DB cũ đang dùng local mà cluster chưa có S3: dừng base backup, status hiện "PITR paused: cần S3", không xoá gì. Base backup local cũ không restore được nữa.
- E2E chỉ có 2 máy (user chọn). Kịch bản CP↔P bị cắt mà P↔S còn sống được giả lập bằng cách **chặn riêng cổng NATS** từ P, WireGuard vẫn để mở. Logic quyết định được phủ bằng unit test dạng bảng.

## Tiêu chí xong

1. Unit: `failoverVerdict` và `agentShouldFence` đúng với bảng kịch bản (xem plan, Phase 6).
2. E2E 2 máy, sampler mỗi giây ghi `pg_is_in_recovery()` trên cả hai node:
   - **CP↔P bị cắt** (chặn cổng NATS từ P): không failover, P vẫn là primary, không bị fence.
   - **P bị cô lập hoàn toàn** (P ở máy không có CP): P bị fence **trước** khi S được promote. Không giây nào có hai giá trị `f`.
   - **Phục hồi**: service primary cũ biến mất, volume nằm trong `volume_gc`, standby mới được tạo.
3. DB không HA có volume, node offline 2 phút rồi về: cùng container ID, `restarts` không đổi.
4. Leader bị kill giữa `pg_promote` và tx metadata: leader mới hoàn tất failover, chỉ notify một lần.
5. PITR: không có file nào của `pitr/` trên node DB sau khi đã ship. Restore tới một thời điểm sau failover thành công trên node khác.

Chi tiết: `plans/261017-db-ha/plan.md`.
