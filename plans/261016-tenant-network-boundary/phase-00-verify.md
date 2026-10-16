# Phase 0 — Xác minh tiền đề

Không sửa code. Kết quả quyết định có đi tiếp theo thiết kế set tĩnh hay không.

## 0a. nft hỗ trợ concat có phép AND trên distro thấp nhất

File test (giống bài đã chạy OK trên nft 1.1.6):

```
table inet t {
  set same { type ifname . ipv4_addr; elements = { "pn1" . 0.0.0.64 } }
  chain forward {
    type filter hook forward priority -5; policy accept;
    oifname . ip saddr & 0.0.255.192 @same accept
    iifname . ip daddr & 0.0.255.192 @same oifname "paaswg0" accept
  }
}
```

Chạy `nft -c -f t.nft` trong `ubuntu:22.04` (nft 1.0.2) và `debian:12` (1.0.6), ví dụ `podman run --rm --cap-add NET_ADMIN -v ...:/t:Z <img> sh -c 'apt-get update -qq && apt-get install -y -qq nftables >/dev/null && nft -c -f /t/t.nft && echo OK'`.

Lưu ý: `nft -c` chạy ở userspace nhưng vẫn hỏi kernel (của host). Bước này chỉ kiểm parser của bản nft cũ, kernel thì không. Kernel chỉ thấy các phép `bitwise` + `lookup` thông thường nên không phải rủi ro.

- OK cả hai → đi tiếp.
- Fail → dừng, quay lại brainstorm để chọn phương án fallback.

## 0b. BuildKit RUN step có đang dùng host network không

Trên một node build:
- `journalctl -u buildkit --no-pager | grep -iE "network|cni"`
- Build thử một Dockerfile có `RUN ip -o -4 addr; ip route; cat /etc/resolv.conf`. Nếu thấy interface/IP của host thì kết luận là host network.

Ghi kết quả vào plan. Nếu RUN đã chạy trong netns riêng thì Phase 4 thu nhỏ còn: đảm bảo bridge tên `pn*` và subnet nằm trong NodeCIDR.

## 0c. Không có project nào chiếm idx 1022

`psql "$PAAS_DATABASE_URL" -c "SELECT max(net_index) FROM projects"` trên mọi deployment đang chạy. Nếu kết quả ≥1022 thì dừng và báo cho người dùng.

## 0d. Resolver của host trên node build

`cat /etc/resolv.conf` (và `resolvectl status` nếu có systemd-resolved). Ghi nhận resolver là loopback, IP public hay IP private. Phase 1 dùng thông tin này để quyết định rule cho phép DNS ra ngoài.

## Xong khi

Bốn kết quả 0a–0d được ghi vào cuối file này.

---

## Kết quả (2026-10-16)

### 0a — ĐẠT
Ruleset mẫu đầy đủ (set `cluster` + set `same` + concat có AND, chain forward/input dự kiến, `meta l4proto { tcp, udp } th dport 53`, `iifname != "pn*"`) được kiểm bằng `nft -c -f -` trong podman:

| Distro | nft | Kết quả |
|---|---|---|
| debian:11 | 0.9.8 | OK |
| ubuntu:22.04 | 1.0.2 | OK |
| debian:12 | 1.0.6 | OK |
| ubuntu:24.04 | 1.0.9 | OK |
| rockylinux:9 | 1.0.9 | OK |
| host Fedora | 1.1.6 | OK |

Đối chứng âm (tham chiếu set không tồn tại) → `RCFAIL`, nên phép kiểm tra là thật.
Giới hạn: `nft -c` hỏi kernel **của host** (7.2), không phải kernel của distro đó. Expression dùng (bitwise, concat lookup, interval set) đã có trong kernel từ 5.x nên rủi ro thấp; Phase 5 trên node thật sẽ xác nhận.
Ruleset mẫu tương đương `internal/network/testdata/ruleset.golden` (thu gọn set `same`).

### 0b — XÁC NHẬN TỪ SOURCE (BuildKit v0.25.1), chưa đo trên node
- `util/network/netproviders/network.go`: mode `auto`/`""` → chỉ dùng CNI khi `cniConfigPath` tồn tại (mặc định `/etc/buildkit/cni.json`), ngược lại `getFallback()` → **host network**, log `"using host network as the default"`. Installer không tạo cni.json → **RUN chạy bằng network của host**.
- Kiểm trên node: `journalctl -u buildkit | grep "using host network"`.
- Phát hiện cho Phase 4:
  - `cniprovider.New`: path kết thúc `.conflist` → `WithConfListFile`, còn lại → `WithConfFile` (một plugin, **không phải conflist**). `confTmpl` là conflist → đặt `cniConfigPath = "/etc/buildkit/cni.conflist"`.
  - Key toml: `networkMode`, `cniConfigPath`, `cniBinaryPath` (struct `NetworkConfig`, nằm trong `[worker.containerd]`).
  - Mode `cni` mà file thiếu → `cniprovider.New` lỗi → buildkitd không start (đúng ý fail-closed, systemd tự restart).
  - DNS ở mode cni (`executor/oci/resolvconf.go`): `/etc/resolv.conf` chỉ có `127.0.0.53` → dùng `/run/systemd/resolve/resolv.conf` (upstream thật), rồi `TransformForLegacyNw` bỏ localhost/IPv6, rỗng thì dùng 8.8.8.8/8.8.4.4. `hostResolvers()` của agent (dns.go:36) đọc **đúng thứ tự file đó** → dùng lại cho rule DNS-exempt ở Phase 1 (bỏ phần `:53`, chỉ giữ IPv4 private).

### 0b — XÁC NHẬN TRÊN NODE
`journalctl -u buildkit` có `level=warning msg="using host network as the default"` (09:42 và 15:11 ngày 16/10). `/etc/buildkit/` chỉ có `buildkitd.toml`, không có file CNI. → RUN step đang chạy trong network của host. Phase 4 là cần thiết.

### 0d — XONG
`/etc/resolv.conf` → stub `127.0.0.53` (systemd-resolved), search `openstacklocal`. Upstream thật: `8.8.8.8` (public).
- Node này không có resolver private → rule DNS-exempt của Phase 1 render rỗng. Vẫn giữ rule đó cho node VPC có resolver private.
- `openstacklocal` → node chạy trên OpenStack, **có metadata service 169.254.169.254** (thường chứa user-data, có thể có secret lúc khởi tạo). Rule drop 169.254/16 có giá trị thật, không phải phòng xa.

### 0c — ĐẠT
`max(net_index) = 2` (đọc `/etc/paas/db.env`). Còn rất xa ngưỡng 1022, migration ở Phase 3 sẽ chạy được.

## Kết luận Phase 0: ĐẠT, đi tiếp Phase 1 và Phase 3.
