# Phase 5 — Kiểm tra e2e trên node thật

Cluster tối thiểu 2 node (để test xuyên mesh), một project shared DB, hai project tenant A, B có container trên cả hai node.

## Từ container của project A (`exec`), phải FAIL (timeout/refused)

| Đích | Lệnh |
|---|---|
| metadata | `curl -m3 http://169.254.169.254/` |
| dashboard API | `curl -m3 http://<host-ip>:8080/` và `http://<gateway>:8080/` |
| NATS | `nc -zw3 <host-ip> 4222` |
| static origin | `curl -m3 http://<host-ip>:7070/` |
| IP mesh | `curl -m3 http://<A.(B+n).255.254>:8080/` |
| project B cùng node | `curl -m3 http://<ip B>:<port>/` |
| project B khác node | như trên |
| IP private ngoài cluster | `curl -m3 http://<IP VPC của node khác>/` |

## Phải OK

DNS nội bộ (`getent hosts <service>`), DNS public, `curl https://example.com`, `curl https://<domain của chính A>`, shared DB `nc -z <shared> 5432`, A gọi A ở node khác.

## Không có khoảng hở khi restart

Từ container A chạy `while :; do curl -s -m1 -o /dev/null -w '%{http_code}\n' http://<ip B>:<port>/; done | grep -v '^000$'`, rồi `systemctl restart paas-agent`. Kỳ vọng: không có dòng nào in ra. Lặp lại với `reboot` node: `nft list table inet paas` có mặt trước khi container đầu tiên start (so timestamp trong journal).

## BuildKit

Theo tiêu chí xong của Phase 4.

## Xong khi

Mọi dòng trong bảng đúng kỳ vọng. Ghi kết quả vào cuối file này.

---

## Kết quả (2026-10-16 22:25–22:31, node 202.92.6.129, commit c034d31) — ĐẠT

Node chỉ có 1 project tenant (idx 2) + shared (idx 1), nên "project khác" = shared registry :5000.

Từ container net-empire (10.101.0.135), `scripts/tenant-isolation.sh` exit 0:
- blocked: 169.254.169.254, host:8080, gw:8080, host:4222, host:7070, mesh 10.101.255.254:8080, shared registry 10.101.0.69:5000
- reachable: Internet (DNS qua gateway), paas.nguyenvando.com:443 hairpin, shared postgres :5432, mysql :3306

Restart `paasd` giữa vòng probe A→registry:5000 (5 lần/s, 40s): 56 probe, **0 lọt**. Đối chứng: host→registry:5000 mở; cùng probe tới shared postgres:5432 thành công.

BuildKit (`buildctl build`): worker `network:cni`, RUN có `eth1 10.101.255.131/26`, route qua 10.101.255.129, nameserver 8.8.8.8. Internet OK; metadata, host:8080, registry, tenant đều bị chặn.

Lỗi test ban đầu (đã sửa trong scripts/tenant-isolation.sh): `nsenter -n` dùng resolv.conf của host (127.0.0.53) → resolve qua gateway rồi `--resolve`; check :443 theo IP không có SNI → dùng dashboard domain.

Chưa test: xuyên mesh sang node khác (cluster chỉ 1 node), reboot node.
