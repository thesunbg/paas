---
title: Biên mạng của tenant
status: implemented
created: 2026-10-16
brainstorm: ../reports/brainstorm-261016-tenant-network-boundary.md
---

# Biên mạng của tenant

Mục tiêu: container tenant và RUN step của BuildKit không tới được host (trừ DNS/80/443), metadata 169.254/16, dải private ngoài cluster, project khác. Restart agent hay reboot đều không để lại khoảng hở.

Ý chính: ruleset **không phụ thuộc state**. Quan hệ `pnN ↔ offset /26` được khai báo tĩnh trong một set `ifname . ipv4_addr`, nên `m.networks` không còn tham gia vào firewall. Sync một lần sau register là xong.

## Phases

| # | Phase | Phụ thuộc | Tier | File |
|---|---|---|---|---|
| 0 | Xác minh tiền đề | — | main/verifier | [phase-00-verify.md](phase-00-verify.md) |
| 1 | Ruleset thuần + golden test | 0 | implementer | [phase-01-ruleset.md](phase-01-ruleset.md) |
| 2 | Vòng đời sync + nft bắt buộc | 1 | implementer | [phase-02-sync-lifecycle.md](phase-02-sync-lifecycle.md) |
| 3 | Dành idx 1022 cho build | 0 | executor | [phase-03-reserve-build-index.md](phase-03-reserve-build-index.md) |
| 4 | BuildKit chạy trên CNI pn1022 | 1,2,3 | implementer | [phase-04-buildkit-cni.md](phase-04-buildkit-cni.md) |
| 5 | Kiểm tra e2e trên node thật | 4 | main | [phase-05-e2e.md](phase-05-e2e.md) |

Phase 1 và 3 có thể chạy song song sau Phase 0.

## KHÔNG làm

- NATS bind 127.0.0.1+mesh: agent register qua NATS trước khi mesh up, `server.Options.Host` chỉ nhận một địa chỉ, và chain input đã chặn container gọi :4222.
- Dựng lại `m.networks` từ đĩa: set tĩnh làm việc này thừa.
- IPv6, allowlist egress cấu hình được: chưa có người dùng thật.
- Không đổi `allowInIptablesForward`, NAT postrouting, DNS server logic (trừ việc nghe thêm trên gateway build nếu Phase 4 cần).

## Tiêu chí xong (toàn plan)

Xem Phase 5. Tóm tắt: từ container và từ RUN step, curl tới 169.254.169.254, host:8080/:4222/:7070, IP mesh, project khác và IP private ngoài cluster đều fail. DNS, Internet, domain của chính mình :443 và shared DB đều OK. Vòng lặp curl xuyên project trong lúc restart agent: 0 lần thành công. `go test ./internal/network/...` xanh với golden.

## Sau review (2026-10-16)

- Sync một lần là chưa đủ: `nft flush ruleset` hay reload firewall làm mất bảng → forward policy accept → mất cách ly tới lần restart; Docker/firewalld dựng lại FORWARD DROP → `allowInIptablesForward` (trước chạy mỗi Attach) không chạy lại → đứt app→DB (lỗi của 0d732b9). Sửa: `EnsureFirewall()` mỗi 30s trong vòng lặp của agent (một goroutine, vẫn không cần mutex).
- Migration 010 vs SERIAL: đã kiểm `max(net_index)=2` trước deploy (phase-00 0c) và paasd đã lên với migration. Giới hạn ~1021 project từng tạo (SERIAL không tái sử dụng) có từ trước, chuyển sang nhóm D/F.
- `e2e.sh` chuyển sang `scripts/tenant-isolation.sh`.

## Rủi ro chính

1. nft cũ không parse được `oifname . ip saddr & mask @set`. Phase 0 chặn sớm. Fallback: rule accept theo từng network + drop chung ở cuối + sync sau Recover (xem brainstorm).
2. Deployment có project `net_index >= 1022`. Phase 0 kiểm tra, migration ở Phase 3 sẽ fail để báo hiệu.
3. Node build cũ (cài trước thay đổi) không có `networkMode = "cni"`. Phase 4 xử lý fail-closed.
