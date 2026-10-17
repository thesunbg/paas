# Brainstorm: H. Niềm tin trong cluster

Ngày: 2026-10-17. Trạng thái: đã chốt hướng, chưa implement.

## Cổng chặn: giả định ban đầu sai

Cổng chặn ban đầu cho rằng "sau nhóm A, NATS không còn mở ra internet, nên có thể hoãn H nếu mọi node cùng VPC". Code hiện tại cho thấy điều này không đúng:

- `config.go:118`: NATS nghe trên `0.0.0.0:4222`. Chain `input` của nhóm A chỉ lọc `iifname "pn*"`, tức là chỉ chặn container, còn từ internet vẫn vào được cổng này.
- `api_nodes.go:146`: lệnh join sinh `nats://<host của PublicURL>:4222`, nên mọi node đều đi qua hostname public.
- Không có TLS. Cluster token và `Env` của tenant (secret) đi plaintext.
- **VPC cũng không cứu được.** Một token chung nghĩa là ai có token thì có quyền root trên cả cluster. Một worker bị escape container hoặc một edge node bị chiếm là đủ để:
  - subscribe `paas.agent.>`: đọc secret của mọi tenant;
  - subscribe `_INBOX.>`: đọc reply của agent gửi về control plane (output của exec, logs);
  - publish `paas.agent.<id bất kỳ>.run`: chạy container tuỳ ý trên mọi node;
  - **trả lời `paas.agent.binary` trước control plane**: gửi URL và SHA256 của một binary độc. Mọi agent sẽ tự update sang nó trong vòng 5 phút (`update.go`), tức là RCE root toàn cluster;
  - register bằng `name` của node khác (`UpsertNode ON CONFLICT (name)`) để chiếm node ID của nó.

Kết luận: **làm ngay, đủ phạm vi.**

## Các phương án đã cân nhắc

| Chủ đề | Phương án | Kết luận |
|---|---|---|
| Phạm vi | Chỉ bind NATS vào IP private và firewall cổng 4222 | Loại. Blast radius vẫn là cả cluster, lỗ self-update vẫn còn |
| Credential | Token riêng cho mỗi node (bearer, server lưu hash) | Loại. Secret vẫn đi trên dây, an toàn phụ thuộc hoàn toàn vào TLS |
| Credential | **NKey**: seed nằm trên node, server chỉ lưu public key, xác thực bằng challenge-response | **Chọn** |
| Cơ chế auth | Danh sách `Users`/`Nkeys` tĩnh, `ReloadOptions` mỗi khi thêm hoặc xoá node | Loại. Phải reload trên mọi control plane HA, dễ race |
| Cơ chế auth | **`CustomClientAuthentication` + `AlwaysEnableNonce`**: tra Postgres lúc connect, gán perms bằng `RegisterUser` | **Chọn**. Có sẵn trong v2.15.1 (`auth.go:46`, `opts.go:548`) |
| TLS | Cert ACME của dashboard | Loại. Không chạy được khi PAAS_TLS tắt, phải xoay cert, HA cần cert chung |
| TLS | **CA riêng**: key mã hoá bằng `secrets.Box` và lưu trong settings; mỗi control plane tự mint cert với SAN cố định `nats.paas.internal` | **Chọn**. Định danh cert không phụ thuộc địa chỉ, HA không cần share file |
| Join qua http | install.sh từ chối `http://` | Chỉ để chặn nhầm cấu hình. Script tải qua http thì MITM sửa được chính đoạn kiểm tra đó. **Chặn thật sự nằm ở server**: không sinh lệnh join khi PublicURL là http |
| Checksum binary, CA pin | Đặt `--binary-sha256` và hash CA vào lệnh join | **Bỏ**. Lệnh join, install.sh, binary và CA đều đến từ cùng một kênh HTTPS, nên checksum hay pin không thêm được gì. Chỉ có giá trị khi checksum đến từ một kênh khác với binary |
| Migration | Agent tự enroll bằng token cũ | Loại. Thêm một đường code legacy chạy lâu dài |
| Migration | Cutover cứng | Loại. Agent cũ không reconnect được và không self-update được. Sau 20s (`LeaseTTL`) HA postgres trên worker tự fence (`fence.go:113`) |
| Migration | **Join lại bằng tay, kèm cờ `PAAS_NATS_LEGACY_TOKEN=1` tạm thời** | **Chọn**. Nợ có chủ ý, điều kiện xoá: không còn node nào thiếu nkey |

## Giải pháp chốt

### 1. Định danh lấy từ subject, không lấy từ payload
NATS không đóng dấu người gửi lên message. Muốn server biết chắc ai gửi thì identity phải nằm trong subject, và permission phải chặn node publish sai subject:
- `paas.agent.register` chuyển thành `paas.node.<id>.register`, `paas.agent.heartbeat` chuyển thành `paas.node.<id>.heartbeat`. Server subscribe `paas.node.*.register|heartbeat` và lấy id từ subject. Field `Token` trong payload bị bỏ.
- Register cập nhật node **theo id**, không upsert theo name nữa. Tạo node mới chỉ xảy ra ở bước enroll.

### 2. Permission theo role (hàm thuần `NodePermissions(id, roles)`)
- Mọi node:
  - sub `_INBOX.<id>.>` (client dùng `nats.CustomInboxPrefix("_INBOX."+id)`);
  - pub `paas.node.<id>.>`, `paas.agent.binary`;
  - allow responses (MaxMsgs 1).
- worker/build:
  - sub `paas.agent.<id>.>`, `paas.dns`, `paas.mesh`, `paas.logs.stop.*`;
  - pub `paas.dns.get`, `paas.mesh.get`, `paas.logs.deploy.*`, `paas.logs.live.*.*`.
- edge:
  - sub `paas.proxy.routes`;
  - pub `paas.proxy.routes.get`, `paas.proxy.stats`, `paas.proxy.wake`.
- Các connection in-process của control plane xác thực bằng secret ngẫu nhiên sinh trong bộ nhớ của từng process (không lưu xuống đĩa) và được full quyền.
- Legacy token khi bật cờ: full quyền, kèm log warn mỗi lần connect.

### 3. Join
1. Admin gọi `POST /api/v1/cluster/join-tokens {roles}`. Token ngẫu nhiên 32B, lưu sha256, hết hạn sau 1h, dùng một lần. Trả 409 nếu PublicURL là http. Lệnh trả về: `curl -fsSL https://dash/install.sh | sudo sh -s -- join --url https://dash --token <t> --roles worker`.
2. install.sh từ chối `--url` không phải https, tải binary từ `<url>/download/paasd`, rồi ghi `PAAS_JOIN_URL` và `PAAS_JOIN_TOKEN` vào env.
3. Agent khởi động lần đầu mà chưa có `/var/lib/paas/identity/`:
   - sinh nkey;
   - gọi `POST /api/v1/cluster/enroll {token, name, nkey}`;
   - server consume token một cách atomic, rồi upsert node theo name: gán nkey, thay key cũ nếu có, kick connection cũ;
   - nhận về `{node_id, nats_url, ca_pem}` và lưu vào `identity/` với quyền 0600.
4. Connect bằng nkey, TLS với RootCAs là CA nhận được và `ServerName=nats.paas.internal`.
- Re-join một host đã có: bước upsert theo name giữ nguyên net_index và instance của node, đây cũng chính là đường migration.

### 4. Thu hồi
- `DELETE /nodes/{id}` xoá nkey, rồi publish `paas.cp.kick {nkey}`. Mọi control plane gọi `DisconnectClientByID` với các cid đã ghi lại lúc auth. Node không subscribe được `paas.cp.*`.

### 5. TLS
- Client TCP bắt buộc dùng TLS. In-process connection không đi qua TLS.
- Route giữa các control plane: TLS với cùng CA, xác thực hai chiều. Password của route là `cluster_token` cũ. Token này từ nay chỉ dùng giữa các control plane và không còn xuất hiện trong lệnh join.
- Cert NATS được mint qua `GetCertificate`, tự mint lại khi còn dưới 30 ngày. Nhờ vậy không cần reload server.

## Rủi ro
- Hành vi nonce + custom auth + `RegisterUser` của v2.15.1 cần một test thật ngay ở phase 1. Nếu không chạy được thì quay về `Nkeys` tĩnh + `ReloadOptions`.
- Có thể có subject bị bỏ sót khi liệt kê permission. Một subject bị deny thì im lặng, nên test e2e phải chạy đủ deploy, logs follow, exec, build, transfer và PITR.
- Rotate CA không nằm trong phạm vi. Nợ có chủ ý: CA hạn 10 năm, rotate bằng tay.
- `paas.proxy.stats` và log line vẫn giả mạo được, nhưng chỉ ảnh hưởng tới số liệu hiển thị. Chấp nhận.

## KHÔNG làm
- Không firewall cổng 4222 theo IP node: auth + TLS đã đủ.
- Không chuyển NATS sang mesh: mesh được bootstrap qua chính NATS, sẽ thành vòng phụ thuộc.
- Không đặt checksum binary hay CA pin trong lệnh join (lý do ở bảng trên).
- Không làm auth callout service hay JWT/account của NATS: chỉ có một account, không cần.
- Không rotate CA.
- Không làm nhóm I (TLS cho public DB).
