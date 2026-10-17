---
title: Niềm tin trong cluster (NKey theo node, permission theo subject, TLS cho NATS, join token dùng một lần)
status: implemented
created: 2026-10-17
brainstorm: ../reports/brainstorm-261017-cluster-trust.md
---

# Niềm tin trong cluster

Mục tiêu:
- Mỗi node xác thực với NATS bằng NKey của riêng nó. Mỗi kết nối được gán permission theo role, ví dụ worker X chỉ subscribe được `paas.agent.X.>` và `_INBOX.X.>`.
- Server biết node nào gửi message nhờ đọc subject (`paas.node.<id>.*`), không tin `NodeID` hay `Token` trong payload nữa.
- NATS bắt buộc TLS, cert do CA nội bộ cấp. Route giữa các control plane cũng đi qua TLS.
- Lệnh join dùng token dùng một lần, hết hạn sau 1h. Server không sinh lệnh join khi PublicURL là http. install.sh từ chối `--url http://`.
- Xoá node thì kết nối NATS của node đó bị ngắt ngay lập tức.
- Cờ `PAAS_NATS_LEGACY_TOKEN=1` giúp cutover không gây gián đoạn. Đây là nợ có chủ ý.

## Phase

| # | File | Nội dung | Phụ thuộc |
|---|---|---|---|
| 1 | [phase-01-bus-auth.md](phase-01-bus-auth.md) | `bus`: authenticator (nkey/in-process/legacy), `NodePermissions`, TLS server/client, kick | — |
| 2 | [phase-02-pki.md](phase-02-pki.md) | CA trong settings (mã hoá), mint cert NATS và route | — |
| 3 | [phase-03-enroll.md](phase-03-enroll.md) | migration, join-tokens, enroll API, kick khi xoá node, đổi subject register/heartbeat phía server | 1, 2 |
| 4 | [phase-04-agent.md](phase-04-agent.md) | identity trên node, enroll lần đầu, connect bằng nkey+TLS, subject mới; proxy standalone | 1, 3 |
| 5 | [phase-05-installer.md](phase-05-installer.md) | install.sh `join --url --token`, từ chối http; CLI `join-command` | 3 |
| 6 | [phase-06-e2e.md](phase-06-e2e.md) | test permission âm, plaintext bị từ chối, token dùng lại bị từ chối, kick, legacy | 1–5 |

Phase 1 và 2 chạy song song được. Phase 4 và 5 cũng chạy song song được sau khi xong phase 3.

## Trạng thái

- Phase 1 và 2 đã xong. `go test -race ./internal/bus/ ./internal/pki/` xanh. `go test ./internal/server/ ./internal/store/` với Postgres thật xanh, 0 skip. `go vet ./...` sạch.
- Phase 2 chỉ làm package `internal/pki`. Việc nối CA vào settings và server chuyển sang phase 3, để không sửa trùng `server.go` khi hai phase chạy song song.
- Phase 6: `TestClusterTrustEndToEnd` và `TestClusterTrustLegacyFlag` (`internal/server/cluster_trust_e2e_test.go`, Postgres thật, enroll qua API, NATS embedded TLS) xanh với `-race`. `scripts/cluster-trust.sh` đã viết, `sh -n` sạch, CHƯA chạy trên cluster 2 VM; bước build và DB PITR để làm tay.

- Phase 3–5 đã xong. Control plane dùng NKey, TLS và join token. Agent có identity riêng; agent chạy trong control plane dùng subject `paas.node.<id>.*` với nkey sentinel `internal:<name>`. Handler cũ chỉ được đăng ký khi bật `PAAS_NATS_LEGACY_TOKEN`. install.sh, CLI, web và docs đã cập nhật.
- Đã sửa các lỗi review tìm ra:
  - enroll trùng tên node của control plane thì trả 409 và không tiêu token;
  - mọi node đều được cấp quyền của agent, kể cả node chỉ có role edge;
  - `Kick` quét `Connz` thay vì giữ map cid;
  - enroll không đi theo redirect;
  - install.sh xoá identity cũ trước khi join.
- Rủi ro chấp nhận: worker vẫn chèn được dòng log giả vào log stream của service khác (chỉ ảnh hưởng tính toàn vẹn của log). Node tự khai address và WireGuard key, như trước.
- Kiểm tra cuối: `go vet ./...` sạch; `go test -count=1 ./...` với Postgres thật xanh, trừ `internal/secrets` `TestRoundTrip` (test flaky có sẵn, không liên quan, khoảng 1/4096 khi ciphertext đã kết thúc bằng `AA`); `-race` cho bus, server, agent, store và pki xanh.
- Còn mở: chạy `scripts/cluster-trust.sh` trên cluster thật.

## Tiêu chí xong (toàn plan)
- `go vet ./...` và `go test ./...` xanh. `go test -race ./internal/bus/ ./internal/server/` xanh.
- Test phase 6 chứng minh được rằng với credential của worker A:
  - không subscribe được `paas.agent.B.>`, `_INBOX.>`, `paas.agent.binary`, `paas.node.B.>`;
  - không publish được `paas.agent.B.run` và `paas.node.B.heartbeat`.
- Kết nối không TLS tới `:4222` bị từ chối.
- `grep -rn "ClusterToken" internal/agent internal/proxy cmd` chỉ còn trong nhánh legacy, và mỗi chỗ có comment ghi điều kiện xoá.
- Chạy tay trên cluster 2 node: join bằng lệnh mới, deploy, logs follow, exec, xoá node. Sau khi xoá, node bị ngắt khỏi NATS trong vòng 1s.

## Quyết định đã chốt
- NKey, không dùng bearer token cho từng node. Dùng `CustomClientAuthentication` + `AlwaysEnableNonce`, tra Postgres mỗi lần connect, không cache.
- Mọi node có đúng một identity. Permission là hợp của các role. Proxy standalone trên cùng host dùng chung seed với agent.
- CA riêng, 10 năm, key mã hoá bằng `secrets.Box`. Cert server có SAN cố định là `nats.paas.internal`, client verify theo tên này dù đang dial địa chỉ nào.
- Join token là sha256 của 32B ngẫu nhiên, TTL 1h, dùng một lần, gắn với danh sách role.
- Không đặt checksum binary hay CA pin trong lệnh join: mọi thứ đến từ cùng một kênh HTTPS.
- `cluster_token` chỉ còn dùng làm password cho route giữa các control plane, và cho cờ legacy.
- Cờ legacy có điều kiện xoá: mọi node đều có `nkey IS NOT NULL`. Dashboard và log warn sẽ hiện những node còn legacy.
