# Brainstorm — A. Biên mạng của tenant

Ngày: 2026-10-16. Trạng thái: đã chốt hướng, chưa implement.

## Vấn đề

- `internal/network/network.go` `Ruleset()` chỉ có chain `forward` (policy accept), không có `input`, không lọc egress → container gọi được 169.254.169.254, :8080/:4222/:7070 của host, dải private của VPC.
- Rule accept/drop được sinh **theo từng network trong `m.networks`**. Agent restart: `agent.go:113` sync với map rỗng → thay bảng cũ (còn trong kernel) bằng bảng thiếu rule drop theo bridge → mở cho tới cuối `Recover` (`runtime.go:759`). Sau reboot: Recover khởi động container trước khi có rule.
- `SyncFirewall` không có lock: hai sync đồng thời có thể áp snapshot cũ sau snapshot mới → bridge mới thiếu rule, policy accept → mở.
- Thiếu `nft` chỉ là warning.
- BuildKit (`install.sh:114`): buildkitd.toml không có `networkMode`, không có `/etc/buildkit/cni.json` → worker containerd chế độ auto gần như chắc rơi về **host network**. RUN step chạy trong netns của host, rule trên `pn*` không có tác dụng. Lỗ này lớn hơn lỗ của container tenant. **Cần xác minh trên node build.**

## Các phương án đã cân nhắc

| Phương án | Ưu | Nhược | Kết luận |
|---|---|---|---|
| Rebuild `m.networks` từ đĩa trước sync đầu | Thay đổi ít về mặt ý tưởng | Thêm code đọc state, state vẫn có thể lệch, Attach vẫn phải sync | Loại |
| Fail-closed (drop chung ở cuối) + sync sau Recover | Thiếu network → bị chặn chứ không bị mở | Vẫn phụ thuộc state, availability lỗi khi lệch | Loại |
| **Set tĩnh `ifname . ipv4_addr`** | Ruleset chỉ phụ thuộc base/node/shared. Xoá được state, xoá sync trong Attach, không còn khoảng hở | Cần nft hỗ trợ concat có phép AND (đã OK trên 1.1.6) | **Chọn** |

Lý do: `saddr & 0.0.255.192` là hằng số theo idx, không phụ thuộc node. Nên quan hệ "bridge pnN ↔ offset /26 của nó" khai báo tĩnh được cho cả 1022 idx. Đây là thắng lợi kiểu "xoá state trước khi validate nó".

## Giải pháp chốt

### 1. Ruleset không phụ thuộc state
- Tách `ruleset(base [2]int, node, shared int) string` thành hàm thuần. `(*Manager).Ruleset()` chỉ còn là wrapper đọc base/node/shared dưới `m.mu`.
- Set tĩnh: `set same { type ifname . ipv4_addr; elements = { "pn1" . 0.0.0.64, … "pn1021" . … } }`. Bỏ `shared` và idx build.
- forward (thứ tự):
  1. `ct state established,related accept`
  2. egress chặn: `iifname "pn*" ip daddr { 169.254.0.0/16, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10 } ip daddr != @cluster drop`. Phải đặt sau rule (1) và trước mọi accept. Vì cluster range nằm trong 10/8 nên dùng điều kiện `!= @cluster`.
  3. vào bridge: `oifname . ip saddr & 0.0.255.192 @same ip saddr @cluster accept` (cùng project trên node khác). Proxy hạ tầng: `oifname "pn*" ip saddr & 0.0.255.192 == 0.0.255.192 ip saddr @cluster accept`. Sau đó là các rule shared (3306/5432) giữ như cũ, cuối cùng là `oifname "pn*" ip saddr @cluster drop`.
  4. ra mesh: `iifname . ip daddr & 0.0.255.192 @same oifname "paaswg0" accept`, rồi rule shared, rồi `iifname "pn*" oifname "paaswg0" drop`.
- input (mới): `type filter hook input priority 0; policy accept;` và chỉ tác động lên `iifname "pn*"`:
  - `ct state established,related accept` (reply của proxy local → container)
  - `udp dport 53 accept`, `tcp dport 53 accept`, đích là gateway của chính bridge đó (hoặc đơn giản là dport 53)
  - `tcp dport { 80, 443 } accept` (hairpin tới domain của chính mình trên host có IP public gắn thẳng vào interface)
  - `drop`
- NAT giữ nguyên.

### 2. Thời điểm sync
- Sync **một lần ngay sau `register`** (base/node/shared đã biết), trước `Recover`. `nft -f` thay bảng atomic → không còn khoảng hở khi restart. Sau reboot thì có rule trước khi container nào được start.
- Sync lại khi `Configure`/`SetShared` thay đổi giá trị.
- **Xoá** `SyncFirewall` trong `Attach` (network.go:203) và cuối `Recover` (runtime.go:759).
- `Restore` vẫn giữ để load CNI + DNS.

### 3. Mutex
- Thêm `fwMu` bọc render và apply trong `SyncFirewall`. Không dùng `m.mu`, vì `Ruleset` đã khoá `m.mu`. Khi Attach không còn sync thì race thực tế hầu như biến mất. Giữ lock vì tốn đúng một dòng.

### 4. nft bắt buộc
- `Run` trả error khi không có `nft` **hoặc** khi lần apply đầu tiên thất bại. Lỗi sync về sau (do Configure) thì log error, giữ bảng cũ.

### 5. BuildKit
- **Bước 0, xác minh:** trên node build, chạy `journalctl -u buildkit | grep -i "network"`, hoặc build thử `RUN ip -o addr; cat /proc/net/route`. Nếu thấy interface của host thì xác nhận RUN đang chạy trong host network.
- Dành idx **1022** cho build: `MaxProjects` thành 1022 cho tenant. Bridge `pn1022`, subnet nằm trong NodeCIDR nên NAT có sẵn. Rule chung `pn*` (input, chặn egress, drop vào/ra mesh) tự áp dụng. Không có phần tử nào trong `@same` → build không tới được project nào.
- buildkitd.toml: `[worker.containerd] networkMode = "cni"`, `cniConfigPath`, `cniBinaryPath`. Agent (hoặc installer) ghi cni.json cho `pn1022`. Node build sinh cấu hình với node index nào thì cần quyết định khi làm plan: subnet phụ thuộc node, mà installer không biết node index → agent ghi file sau register rồi restart buildkit, **hoặc** dùng subnet cố định trong infra slot. Không dùng infra slot vì nguồn infra được tin cậy.
- DNS cho build: BuildKit lấy resolv.conf của host. Nếu resolver là IP private (VPC 10.0.0.2, 169.254.169.253) thì sẽ bị rule egress chặn. ~~Trỏ DNS của build về gateway `pn1022`~~ → đổi khi lập plan: miễn trừ cổng 53 tới các resolver private của host ngay trong ruleset, vì gateway phụ thuộc node nên phải để agent sinh buildkitd.toml, phức tạp hơn (xem phase-01).

## Rủi ro / phụ thuộc

- **Phiên bản nft:** concat có phép AND đã OK trên 1.1.6. Phải `nft -c` trên distro thấp nhất được hỗ trợ (Ubuntu 22.04 dùng 1.0.2, Debian 12 dùng 1.0.6) trước khi bắt đầu. Fail thì quay về phương án "fail-closed + reorder".
- **idx 1022:** `projects.net_index` là SERIAL, không bao giờ tái sử dụng. Cần (a) kiểm tra deployment hiện có không có project nào ≥1022, (b) server từ chối tạo project khi `net_index >= 1022`.
- **Chặn RFC1918 làm hỏng use case** tenant gọi DB/dịch vụ trong VPC riêng. Chấp nhận, mặc định chặn. Allowlist theo cấu hình để sau, khi có người thật cần.
- **Hairpin 80/443:** `:8080` vẫn bị chặn. Dashboard vẫn tới được qua proxy bằng domain, vì nó vốn đã public.
- Rule của iptables-DOCKER-USER (`allowInIptablesForward`) không đổi.

## KHÔNG làm (cố ý loại)

- NATS bind 127.0.0.1+mesh: agent register qua NATS trước khi mesh up (agent.go:95), `server.Options.Host` chỉ nhận một địa chỉ, và chain input đã chặn container gọi :4222. Phơi NATS ra Internet là vấn đề riêng, đã có token.
- Dựng lại `m.networks` từ đĩa: không cần nữa nhờ set tĩnh.
- IPv6: CNI chỉ cấp IPv4, không có route v6.
- Allowlist egress cấu hình được: chưa có người dùng thật.

## Tiêu chí xong

- Từ trong container của project: `curl` tới 169.254.169.254, `<host>:8080`, `:4222`, `:7070`, IP mesh của node, IP container của project khác (cùng node và khác node), và một IP private ngoài cluster đều **fail**. DNS nội bộ, Internet, `<own-domain>:443` và shared DB 3306/5432 đều **OK**.
- Từ RUN step của BuildKit: cùng danh sách fail ở trên. `apt-get`/`npm install` OK.
- Restart agent trong khi chạy vòng lặp `curl` khác project: 0 lần thành công. Reboot node: rule có trước khi container đầu tiên start.
- `TestRuleset`: golden string cho `ruleset([2]int{10,100}, 1, 5)`, có thêm một case `shared=0`. Thêm test `nft -c` chạy qua `unshare -rn`, skip nếu không có nft hoặc không có quyền.
- Agent không khởi động khi thiếu nft.

## Bước tiếp

0. Xác minh network mode của BuildKit và kiểm `nft -c` trên các distro thấp nhất (song song).
1. Ruleset + test golden.
2. Đổi thời điểm sync, mutex, nft bắt buộc.
3. Giữ idx 1022 (server + agent).
4. CNI cho BuildKit + DNS.
5. Chạy kiểm tra e2e theo tiêu chí ở trên.
