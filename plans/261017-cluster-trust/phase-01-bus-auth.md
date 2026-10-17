# Phase 1: auth và TLS trong `internal/bus`

## Việc
1. Thêm `github.com/nats-io/nkeys` thành dependency trực tiếp (hiện đang là indirect v0.4.16).
2. `bus.NodePermissions(nodeID string, roles []string) *server.Permissions`, viết thành hàm thuần (bảng ở brainstorm §2):
   - mọi node: sub `_INBOX.<id>.>`; pub `paas.node.<id>.>`, `paas.agent.binary`; `Response: &server.ResponsePermission{MaxMsgs: 1}`;
   - `worker` hoặc `build`: sub `paas.agent.<id>.>`, `paas.dns`, `paas.mesh`, `paas.logs.stop.*`; pub `paas.dns.get`, `paas.mesh.get`, `paas.logs.deploy.*`, `paas.logs.live.*.*`;
   - `edge`: sub `paas.proxy.routes`; pub `paas.proxy.routes.get`, `paas.proxy.stats`, `paas.proxy.wake`;
   - roles rỗng thì coi như `worker`, khớp với mặc định của installer.
3. `bus.Auth` implement `server.Authentication`:
   - `Internal string`: secret in-process. Khớp (so sánh constant-time) thì gọi `RegisterUser` với permission nil (full quyền).
   - `Lookup func(nkey string) (nodeID string, roles []string, ok bool)`: khi `opts.Nkey != ""`, verify `opts.Sig` trên `GetNonce()` bằng `nkeys.FromPublicKey(...).Verify`. Sig là base64 raw-url, cần đối chiếu cách nats-server tự verify (`auth.go`, tìm `Verify(`). Sau đó gọi `Lookup` rồi `RegisterUser(&server.User{Username: nkey, Permissions: NodePermissions(...)})`. Ghi `GetID()` vào `map[nkey][]uint64`, có mutex bảo vệ.
   - `Legacy string`: chỉ có giá trị khi cờ bật. Khớp thì full quyền và `slog.Warn`. Comment tại chỗ: `// Legacy: remove when no node has nkey IS NULL (plan 261017-cluster-trust).`
   - `Kind() != server.CLIENT` thì từ chối. Route không đi qua đây.
   - Phải cân nhắc việc map cid lớn dần, ví dụ dọn khi `Kick`. Không cần hook disconnect: gọi `DisconnectClientByID` lên một cid đã chết chỉ trả lỗi, bỏ qua lỗi đó.
4. `(*Auth).Kick(ns *server.Server, nkey string)`: duyệt các cid của nkey rồi gọi `ns.DisconnectClientByID`.
5. `StartServer`: đổi signature, nhận `auth *Auth` và `tlsCfg *tls.Config`, bỏ tham số `token`. Đặt `opts.CustomClientAuthentication = auth`, `opts.AlwaysEnableNonce = true`, `opts.TLSConfig = tlsCfg`, `opts.TLSTimeout = 2`. Route nhận thêm `TLSConfig` và giữ `Username/Password` (cluster_token).
6. Client:
   - `ConnectNode(url, seed []byte, nodeID string, ca *x509.CertPool, name string)`: `nats.Nkey(pub, kp.Sign)`, `nats.Secure(&tls.Config{RootCAs: ca, ServerName: "nats.paas.internal", MinVersion: tls.VersionTLS12})`, `nats.CustomInboxPrefix("_INBOX."+nodeID)`, cùng các option reconnect hiện có.
   - `ConnectInProcess` chuyển sang dùng secret internal (vẫn qua `nats.Token`).
   - `Connect(url, token, name)` cũ chỉ còn cho legacy, kèm comment điều kiện xoá giống trên.
   - Lưu ý: nodeID chỉ có sau khi enroll, nên inbox prefix cần nodeID ngay lúc dial. Identity file (phase 4) phải chứa sẵn nodeID.
7. Test trong `internal/bus/auth_test.go`, dựng server thật trên `127.0.0.1:0` với TLS từ một CA tạo ngay trong test:
   - nkey đúng thì connect được, nkey lạ thì bị từ chối, sig sai thì bị từ chối;
   - worker A: subscribe `paas.agent.B.x` nhận permission violation (dùng `nats.ErrorHandler` để bắt `-ERR 'Permissions Violation'`); publish `paas.agent.B.run` thì subscriber full quyền không nhận được gì;
   - request/reply: control plane gửi request tới `paas.agent.A.ping` thì A trả lời được (allow responses);
   - kết nối plain (không TLS) bị từ chối;
   - sau `Kick` thì client A nhận disconnect;
   - test bảng cho `NodePermissions` (worker, edge, worker+edge).

## Tiêu chí xong
- `go test -race ./internal/bus/` xanh.
- Nếu nonce cộng custom auth không chạy được trên v2.15.1 thì **dừng và báo lại**, không tự chuyển sang phương án khác.

## KHÔNG làm
- JWT, account, auth callout của NATS.
- Cache kết quả `Lookup`.
- Sửa caller ở `server` hay `agent`, việc này để phase 3 và 4. Phase này chỉ cần các caller compile được: sửa tối thiểu, truyền nil hoặc giá trị tạm, và ghi chú lại.
