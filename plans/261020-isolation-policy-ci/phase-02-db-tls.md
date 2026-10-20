# Phase 2 — TLS bắt buộc cho DB public

File: `/home/hwng/github_projects/paas/internal/proxy/tcp.go`, `internal/proxy/proxy.go` (`dbTLSConfig`), `internal/server/dbaas.go` (`tcpMode`, `tcpRoutes`, `externalURL`, API bật public ~:160-199), test cạnh từng file.

## Việc
1. **postgres**: nhánh "A plain startup message" → `return`, trừ CancelRequest (`length==16 && code==80877102`) thì forward như cũ. Thêm hằng `pgCancelRequest`.
2. **mode `tls`** (redis, mongo): `tlsCfg == nil` → `return`, không cho plaintext.
3. **Không TLS ở edge** (`dbTLS` nil): `tcpManager.apply` không mở listener, `slog.Warn`. API bật public port trả 409 "cần HTTPS/PAAS_TLS" khi `!s.tlsAtEdge()`. Ghi chú trong comment: `tlsAtEdge` đọc `PublicURL` của control plane, edge đa node có thể lệch.
4. **`externalURL`**: bỏ nhánh `sslmode=disable`; Postgres luôn `require`. Redis/Mongo giữ như hiện tại (đã chỉ đặt khi TLS; vì public chỉ mở khi TLS nên luôn đúng).
5. **MySQL (`plain`)**: API từ chối bật public (409, nói rõ lý do). `tcpRoutes` bỏ route mode `plain`. DB đang public: khi nâng cấp, log Warn mỗi DB bị đóng; chọn trong phase này giữa đặt `PublicPort=0` bằng migration hay chỉ im port (khuyến nghị: im port + hiện cảnh báo trong dashboard `tls:false`/`blocked`).
6. Test: postgres plain startup bị đóng; SSLRequest + TLS qua; CancelRequest qua; direct TLS qua; `tlsCfg==nil` → không listener; redis/mongo plaintext bị đóng; `externalURL` không bao giờ có `sslmode=disable`.

## Xong khi
`go test ./internal/proxy/... ./internal/server/...` xanh; `psql sslmode=disable` vào port public bị từ chối.

## KHÔNG làm
Không viết STARTTLS MySQL; không `require_secure_transport`; không `verify-full`; không per-IP cap (thuộc nhóm G).
