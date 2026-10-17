# Phase 6: kiểm chứng

## Việc
1. Test Go tích hợp (`internal/server`, dùng Postgres thật và NATS embedded có TLS):
   - enroll worker A và worker B qua API rồi connect bằng `bus.ConnectNode`;
   - với credential của A: sub `paas.agent.B.>`, `_INBOX.>`, `paas.agent.binary`, `paas.node.B.>`, `paas.cp.kick` đều bị permission violation; pub `paas.agent.B.run` và `paas.node.B.heartbeat` không tới được subscriber;
   - control plane gọi `requestAgent(A, ...)` thì A trả lời được; heartbeat của A trên `paas.node.A.heartbeat` cập nhật `last_seen_at` của A;
   - DELETE node A thì client A bị disconnect và reconnect bị từ chối;
   - bật cờ legacy thì client dùng cluster_token connect được; tắt cờ thì bị từ chối.
2. Script `scripts/cluster-trust.sh`, chạy tay trên 2 VM theo mẫu `scripts/tenant-isolation.sh`:
   - `nc cp 4222`: dòng INFO phải có `"tls_required":true`, và gửi `CONNECT {}` dạng plain thì bị đóng kết nối (NATS gửi INFO trước khi bắt tay TLS);
   - join node 2 bằng lệnh mới, deploy một app, chạy `paas logs -f`, `paas exec`, một build, một DB có PITR;
   - chạy lại lệnh join cũ với token đã dùng thì phải fail;
   - xoá node 2, sau đó `journalctl -u paasd` trên node 2 phải có dòng authorization violation.
3. Ghi kết quả vào phần "Trạng thái" của plan.md.

## Tiêu chí xong
- Test tích hợp xanh với `-race`.
- Script chạy xanh trên cluster thật, hoặc ghi rõ bước nào chưa chạy được.

## KHÔNG làm
- Load test cho auth.
- Fuzz cho enroll.
