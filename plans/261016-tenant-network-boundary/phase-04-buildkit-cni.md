# Phase 4 — BuildKit chạy trên CNI pn1022

Điều kiện tiên quyết: Phase 0b xác nhận RUN đang dùng host network. Nếu không thì xem ghi chú phạm vi thu nhỏ trong Phase 0.

File: `internal/installer/install.sh`, `internal/network/network.go` (hoặc file mới `internal/network/build.go`), `internal/agent/agent.go`.

## Thay đổi

1. **Installer** (`install.sh` ~114), thêm vào `[worker.containerd]`:
   ```toml
   networkMode = "cni"
   cniConfigPath = "/etc/buildkit/cni.conflist"
   cniBinaryPath = "/opt/cni/bin"
   ```
   Lúc này file conflist chưa có, buildkitd sẽ fail rồi được systemd khởi động lại (`Restart=always`). Agent ghi file xong thì nó chạy được. Kiểm tra unit có `RestartSec` hợp lý không, tránh hit start-limit. Nếu cần thì thêm `StartLimitIntervalSec=0`.
2. **Agent sinh `/etc/buildkit/cni.conflist`** (role build, sau register vì subnet phụ thuộc node index):
   - Dùng lại `confTmpl` với `Index=BuildNetIndex`, `Bridge="pn1022"`, `Subnet/Gateway = subnetFor(base,node,BuildNetIndex)`, IPAM dir riêng `dataDir/cni/ipam-build`. Đã xác minh (Phase 0b): đuôi `.conflist` → BuildKit đọc dạng conflist, khớp `confTmpl`.
   - Ghi atomic (tmp + rename). Chỉ khi nội dung đổi mới chạy `systemctl restart buildkit` (lấy theo tiền lệ gọi systemctl trong repo nếu có). Một hàm duy nhất, gọi từ `Run` ngay sau `SyncFirewall`.
3. **Fail-closed cho node cũ**: khi role build bật mà `/etc/buildkit/buildkitd.toml` không chứa `networkMode = "cni"` thì `slog.Error` kèm hướng dẫn chạy lại installer, và `a.builder = nil` (không nhận job build). Cách kiểm tra là đọc file và so chuỗi, không cần parser TOML.
4. **DNS cho build**: RUN dùng resolv.conf của host. Với systemd-resolved, BuildKit dùng `/run/systemd/resolve/resolv.conf`, tức upstream thật. Còn lại chỉ loopback thì BuildKit thay bằng 8.8.8.8 (public, đi qua NAT). Xem Phase 0b. Resolver private thì đã được miễn trừ ở Phase 1 (DNS-exempt). Không cần nghe DNS trên gateway pn1022.
5. Rule firewall không cần làm gì thêm: `pn1022` khớp `pn*` nên tự nhận chain input, rule chặn egress, drop vào mesh và drop @cluster. Nó không có phần tử trong `@same` nên không tới được project nào. Subnet nằm trong NodeCIDR nên NAT masquerade có sẵn.

## KHÔNG làm

- Không chuyển việc sinh buildkitd.toml sang agent (mirror vẫn do installer quản lý).
- Không dùng infra slot (nguồn 0.0.255.192 được tin cậy để tới mọi project).
- Không thêm network riêng cho từng build.

## Xong khi

- Trên node build: build Dockerfile có `RUN ip -o -4 addr` → thấy IP `A.(B+n).255.1xx` trên eth0, không phải IP host.
- `RUN apt-get update` hoặc `npm ci` OK.
- `RUN curl -m3 169.254.169.254` và `RUN curl -m3 <host>:8080` fail (build lỗi đúng như mong đợi).
- Node có toml cũ: agent log error và không nhận job build.

---

## Kết quả (2026-10-16) — CODE XONG, chưa chạy trên node

- `network.confList(idx)` tách ra từ `network()`; `BuildConfList()` render conflist cho pn1022 (cùng template, cùng IPAM dir — host-local tách theo tên network `paas-1022`).
- `internal/agent/buildnet.go`: `setupBuildNetwork()` chạy sau `SyncFirewall`, trước Recover. Toml thiếu `networkMode = "cni"` → lỗi. Ghi `cni.conflist` atomic, restart buildkit chỉ khi nội dung đổi. Test: `buildnet_test.go`.
- **Lệch plan:** node toml cũ KHÔNG đặt `a.builder = nil` (sẽ tắt luôn `serveStaticOrigin` → static site trên node sập). Thay bằng `a.buildBlocked`: `handleBuild` trả lỗi rõ ràng, static origin/detect/push vẫn chạy.
- Installer: thêm `networkMode/cniConfigPath/cniBinaryPath`, unit có `StartLimitIntervalSec=0` + `RestartSec=5` (buildkitd fail tới khi agent ghi conflist). Installer đã `daemon-reload` + restart paasd, nên redeploy tự chuyển node cũ sang CNI.
- `go test ./...` xanh. Chưa chạy được buildkitd thật trên máy dev (không có root).
