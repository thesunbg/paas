# Brainstorm: D3 — fencing cho leader

## Vấn đề

- `leaderLoop` (`internal/server/server.go:354`) giữ leader bằng session advisory lock `7313001` trên một conn riêng, cứ 5 giây ping một lần.
- Conn đó đứt (Postgres restart, admin kill, mạng chập chờn) thì backend chết và Postgres **nhả lock ngay**. Control plane khác lấy được lock, rồi chạy `FailStuckDeployments`.
- Leader cũ phải tới 10 giây sau mới biết mình mất lock. Kể cả khi biết rồi:
  - `runDeployment` chạy bằng `context.WithoutCancel(ctx)` (`deploy.go:67`), nên build, rollout và các lệnh ghi DB vẫn tiếp tục.
  - Các goroutine trong cây leader dùng `context.Background()`: tạo lại standby trong `haTick`, base backup PITR sau failover, `cleanupStatic`, `removeVolumesWhenOnline`.
- Hậu quả cụ thể:
  - Leader mới đánh deployment D thành `failed`. Leader cũ sau đó ghi D thành `active` và set current deployment.
  - Leader mới claim D2 của cùng service, nên hai rollout chồng nhau.
  - Hai leader cùng failover một database.
- `FailStuckDeployments` không reset status của service. Service sẽ kẹt ở `deploying` mãi mãi.
- `CancelOlderQueued` huỷ **mọi** deployment queued cũ hơn. Push webhook tới sau một lệnh rollback đang queued thì rollback bị huỷ. Deploy manual một commit cũ cũng huỷ luôn push mới hơn đang chờ.
- `startInstance` đọc `NodeCommitted`, chạy `scheduler.Pick`, rồi mới `CreateInstance`, mà không có lock nào bao quanh. Có tới 8 deploy song song cộng với reconcile, nên node có thể bị overcommit.

## Sự thật đã xác minh

- Session lock đã chống được trường hợp process bị đóng băng (GC, SIGSTOP, VM pause): TCP vẫn sống thì lock vẫn được giữ. Lỗ thật chỉ có một: **conn chết làm lock được nhả, nhưng công việc của leader cũ không dừng**.
- Heartbeat (`reconcile.go:40-47`) dừng mọi container không có row trong `instances`. Vì vậy container do leader cũ khởi chạy mà không ghi được row sẽ tự bị dọn.
- Row instance ở `starting` không bao giờ được heartbeat cập nhật (`ApplyContainerStatus` chỉ đổi từ `running` hoặc `failed`), và vẫn bị tính vào committed. Một khi huỷ ctx giữa rollout thì sẽ sinh ra các row như vậy, nên phải dọn chúng khi takeover.
- `scheduler` import `store`, nên store không gọi được `scheduler.Pick`. Phải truyền closure vào.
- Rollback đi qua `redeploy`, không gọi `CancelOlderQueued`. Rollback bị huỷ là do webhook tới **sau** đó.
- Các `WithoutCancel` trong handler API (delete, stop, start, backup, template...) chạy trên mọi control plane để chống client ngắt kết nối. Chúng **không** thuộc cây leader nên không đổi.

## Quyết định (đã chốt với user)

| # | Quyết định | Lý do |
|---|---|---|
| 1 | Bảng `leader(epoch)`. Leader mới bump epoch dưới `pg_advisory_xact_lock(K_FENCE)` (exclusive). | Một nguồn sự thật cho generation. |
| 2 | Ctx của leader mang epoch (`store.WithEpoch`). `store.q(ctx)` trả về executor fenced khi ctx có epoch, và pool khi không có. | Mọi lệnh ghi trong cây leader được fence tự động. Đường API trên follower không bị ảnh hưởng. |
| 3 | Lệnh fenced = một batch pgx (implicit tx, 1 round trip): `SELECT paas_fence($epoch)` rồi tới câu lệnh. `paas_fence` (plpgsql) lấy `pg_advisory_xact_lock_shared(K_FENCE)`, sau đó **ở một statement riêng** đọc epoch, lệch thì `RAISE`. `Begin` fenced chạy `paas_fence` làm câu đầu của tx. | Fencing thật: bump phải chờ các tx cũ xong, và sau bump mọi tx cũ đều fail. Advisory lock không ghi tuple, nên không bị multixact phình như `FOR SHARE` trên một row nóng. |
| 4 | `requestAgent` gọi `store.CheckFence(ctx)` trước khi gửi (không làm gì nếu ctx không có epoch). Huỷ ctx leader thì request đang bay bị huỷ theo. | Đủ cho tiêu chí đã đặt. Container lọt qua thì heartbeat dọn. Không đổi proto. |
| 5 | Bỏ `WithoutCancel` ở `deploy.go:67`. Trong cây leader, `context.Background()` đổi thành ctx của leader. | Mất lock là dừng việc. |
| 6 | `FailStuckDeployments` làm trong một CTE: deployment đã là `current_deployment_id` thì đánh `active`, còn lại đánh `failed`. Service nào đang `deploying` thì đặt `running` nếu có current, không có thì `failed`. Xoá các instance `starting` thuộc những deployment bị đánh failed. | Cùng ngữ nghĩa với `failDeployment`. Heartbeat sẽ dọn container của các row vừa xoá. |
| 7 | `CancelOlderQueued` chỉ được gọi khi trigger=`webhook`, và chỉ huỷ các deployment trigger=`webhook`. | Push mới thay push cũ. Rollback, manual và system không bị huỷ. |
| 8 | `store.PlaceInstance(ctx, inst, pick)`: trong một tx lấy `pg_advisory_xact_lock(hashtext('placement'))`, đọc `Nodes` và committed, gọi `pick`, rồi INSERT. | Đoạn critical chỉ có DB, vài ms. Lock tự nhả, không cần retry. |

## Phương án đã loại

- **Chỉ guard khoảng 10 câu UPDATE chuyển trạng thái.** Dễ sót. `(SELECT epoch)` trong UPDATE ở READ COMMITTED dùng snapshot đầu statement, nên vẫn hở một cửa sổ ghi đè kết quả FailStuck.
- **`SELECT epoch` rồi mới làm (check-then-act).** Có TOCTOU, không phải fencing.
- **`FOR SHARE` trên row `leader`.** Đúng, nhưng mỗi lệnh ghi đều tạo multixact trên một row nóng (metricsLoop ghi liên tục).
- **Agent tự từ chối epoch cũ.** Phải đổi proto và agent, lệnh API không có epoch phải đi đường riêng, và phải broadcast epoch. Hiện chưa có hỏng hóc nào mà heartbeat cộng check trước khi gửi không xử lý được.
- **Row lock theo node cộng retry cho capacity.** Song song tốt hơn, nhưng cluster nhỏ không cần và phải viết thêm vòng retry.
- **Bỏ `WithoutCancel` trong handler API.** Không thuộc cây leader. Bỏ đi thì client ngắt kết nối là thao tác chết giữa chừng.

## Rủi ro và nợ ghi nhận

- Check trước `requestAgent` vẫn hở một cửa sổ vài ms. Lệnh `stop`/`exec` (vd. `pg_promote`) lọt qua cửa sổ đó thì không có gì dọn. Điều kiện để làm fencing ở agent: có sự cố thật, hoặc có thêm lệnh agent nào không idempotent.
- Leader cũ bị huỷ giữa rollout để lại trạng thái dở: có instance mới, có instance cũ. Leader mới xử lý bằng FailStuck cộng reconcile. Deployment bị đánh `failed` chứ không được chạy lại.
- `removeVolumesWhenOnline` chuyển sang ctx leader thì bị huỷ khi mất lock, và volume trên node cũ bị bỏ lại. Hôm nay restart cũng gây ra đúng việc này. Không làm gì thêm.
- Fence thêm một statement vào mỗi batch (vẫn 1 round trip). Nếu metricsLoop chậm đi đo được thì mới cân nhắc tách riêng.
- Tx fenced của leader cũ bị treo thì bump của leader mới phải chờ. Câu lệnh của leader đều ngắn. Không đặt `lock_timeout`.
- Không thuộc D3:
  - Job run không được tính vào committed.
  - Conn half-open thì Postgres giữ lock tới khi TCP keepalive hết hạn, nên cluster có thể không có leader trong thời gian đó. Đây là vấn đề availability, không phải safety. Cách sửa: `tcp_keepalives_*` phía server.
  - Webhook tới sau rollback vẫn deploy commit mới. Đúng ý định "push mới". Pause autodeploy sau khi rollback là feature khác.

## Tiêu chí xong

1. Leader cũ (ctx mang epoch n, sau khi đã bump lên n+1) gọi `runDeployment` hoặc `promoteStandby` hoặc ghi store bất kỳ thì nhận `ErrFenced`. DB không đổi, và không lệnh agent nào được gửi.
2. Rollback đang queued, webhook tới sau, thì rollback vẫn ở trạng thái `queued`.
3. Chạy song song nhiều lần placement vượt capacity của một node thì số instance được tạo đúng bằng số vừa capacity.
4. FailStuck: service về đúng status, các instance `starting` mồ côi bị xoá.

Chi tiết xem `plans/261017-leader-fencing/plan.md`.
