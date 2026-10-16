# Phase 1 — Ruleset thuần + golden test

File: `internal/network/network.go`, `internal/network/dns_test.go` (hoặc `network_test.go` mới), `internal/network/testdata/ruleset.golden`.

## Thay đổi

1. Hàm thuần `ruleset(base [2]int, node, shared int, resolvers []string) string`. `(*Manager).Ruleset()` khoá `m.mu`, lấy `base/node/shared`, lấy `m.DNS.upstreams` (hoặc `hostResolvers()`), rồi gọi `ruleset`. **Không đọc `m.networks`.**
2. Set tĩnh trong `table inet paas`:
   ```
   set same {
     type ifname . ipv4_addr
     elements = { "pn1" . 0.0.0.64, "pn2" . 0.0.0.128, … }   // idx 1..BuildNetIndex-1, bỏ shared
   }
   ```
   Mỗi phần tử được tính bằng `projectOffset(i)`. Không đưa idx build (1022) vào set.
3. Chain `forward` (policy accept, priority -5), theo thứ tự:
   ```
   ct state established,related accept
   iifname "pn*" ip daddr { 169.254.0.0/16, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10 } ip daddr != @cluster [DNS-exempt] drop
   # vào bridge
   oifname "pn*" ip saddr & 0.0.255.192 == 0.0.255.192 ip saddr @cluster accept     # proxy hạ tầng
   oifname . ip saddr & 0.0.255.192 @same ip saddr @cluster accept                  # cùng project, node khác
   oifname "pn<shared>" tcp dport { 3306, 5432 } ip saddr @cluster accept           # nếu shared > 0
   oifname "pn*" ip saddr @cluster drop
   # ra mesh
   iifname . ip daddr & 0.0.255.192 @same oifname "paaswg0" accept
   iifname "pn*" oifname "paaswg0" ip daddr & 0.0.255.192 == 0.0.<shared> accept     # nếu shared > 0
   iifname "pn*" oifname "paaswg0" drop
   ```
   - Rule shared hiện tại "infra → shared accept" đã được rule proxy hạ tầng chung bao phủ. Rule "shared drop" được bao bởi drop chung. Kiểm tra lại cẩn thận: hành vi của bridge shared phải tương đương bản cũ.
   - Vì tối đa 1 shared nên rule shared vẫn render động, giá trị đó đã biết sau register.
   - **DNS-exempt:** resolver của host có IP private (Phase 0d) thì bị rule drop chặn, mà BuildKit RUN dùng resolv.conf của host. Thêm một rule trước rule drop: `iifname "pn*" ip daddr { <resolver private> } meta l4proto { tcp, udp } th dport 53 accept`. Chỉ render khi danh sách resolver private khác rỗng. Bỏ qua resolver loopback.
4. Chain `input` mới:
   ```
   chain input {
     type filter hook input priority 0; policy accept;
     iifname != "pn*" accept
     ct state established,related accept
     meta l4proto { tcp, udp } th dport 53 accept
     tcp dport { 80, 443 } accept
     drop
   }
   ```
5. Table NAT giữ nguyên.

## Test

- `TestRulesetGolden`: `ruleset([2]int{10,100}, 1, 5, []string{"10.0.0.2"})` so với `testdata/ruleset.golden`. Thêm cờ `-update` để ghi lại file. Thêm một case `shared=0, resolvers=nil` với golden riêng. (Set có ~1020 phần tử nên golden phải là file, không để inline.)
- `TestRulesetNftCheck`: ghi ruleset ra stdin của `unshare -rn nft -c -f -`. `t.Skip` khi thiếu `nft` hoặc `unshare`, hoặc khi gặp EPERM. Test này bắt lỗi cú pháp mà golden không bắt được.
- Xoá `TestRulesetIsolation` (bị golden thay thế), hoặc giữ vài `strings.Contains` cho rule then chốt như rule drop 169.254 và chain input. Chọn một, đừng giữ cả hai.

## KHÔNG làm

- Không đổi `SyncFirewall`/lifecycle (Phase 2).
- Không cấu hình được danh sách dải egress.

## Xong khi

`go test ./internal/network/...` xanh. Test `nft -c` chạy (không skip) trên máy dev. Đã tự đọc golden: không có rule nào phụ thuộc `m.networks`.
