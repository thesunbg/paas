# Phase 5: installer và CLI

## Việc
1. `internal/installer/install.sh`, chế độ `join`:
   - flag mới `--url URL` (dashboard, bắt buộc https) và `--token TOKEN` (join token). Bỏ `--server`.
   - `case "$URL" in https://*) ;; *) die "join needs an https:// --url";; esac`. Comment rằng đây chỉ là chặn nhầm cấu hình, chặn thật nằm ở server.
   - nếu không truyền `--binary` thì `BINARY` mặc định là `$URL/download/paasd`. `--binary` truyền vào mà bắt đầu bằng `http://` thì cũng `die`.
   - env ghi ra: `PAAS_JOIN_URL`, `PAAS_JOIN_TOKEN`, `PAAS_ROLES`, `PAAS_ACME_EMAIL`. Bỏ `PAAS_NATS_URL` và `PAAS_CLUSTER_TOKEN`.
   - cập nhật comment usage ở đầu file (dòng 8, 16, 17).
2. `cmd/paas/main.go`: `join-command [--roles r1,r2]` gọi `POST /cluster/join-tokens` rồi in command kèm thời điểm hết hạn. Cập nhật help ở dòng 187.
3. Web UI: tìm nơi đang gọi `/cluster/join` (`grep -rn "cluster/join" web internal/webui`), chuyển sang POST, và hiện thông điệp 409 khi PublicURL là http.
4. README và `docs/API.md`: cập nhật lệnh join và hai endpoint mới. Thêm một đoạn ngắn về quy trình migration: bật `PAAS_NATS_LEGACY_TOKEN=1`, nâng cấp control plane, chạy lệnh join trên từng node, rồi tắt cờ khi dashboard không còn node legacy.

## Tiêu chí xong
- `sh -n internal/installer/install.sh` OK. `shellcheck` sạch nếu máy có cài.
- `grep -n "PAAS_CLUSTER_TOKEN\|--server" internal/installer/install.sh` không còn kết quả nào ở chế độ join.

## KHÔNG làm
- Checksum binary hay CA pin trong lệnh.
- Tải install.sh từ GitHub trong lệnh join.
