# Nhóm I — Chính sách cách ly và CI (brainstorm 2026-10-20)

5 việc. Quyết định đã chốt với user. Không implement ở đây.

## Quyết định

| # | Việc | Chốt |
|---|---|---|
| 1 | deploy.yml + server | User `deploy` riêng, key-only, sudo đúng 1 wrapper root-owned. Ghim host key. host/port/user vào vars. Bỏ sshpass. Chấp nhận: deploy key ≈ root (xem Rủi ro). |
| 2 | sandboxed → runc âm thầm | `runtimeFor` trả lỗi, không Warn. Ghi tài liệu: `standard` KHÔNG phải ranh giới cho code không tin cậy. Không làm userns (workstream riêng). |
| 3 | TLS cho DB public | Postgres: từ chối startup plaintext (cho phép CancelRequest). TLS-less edge: không mở port, API từ chối bật public. externalURL luôn `sslmode=require`. MySQL: chặn public. |
| 4 | Cache mount Dockerfile user | Viết lại `id=<project>/<id>` hoặc thêm `id=<project><target>`, fail-closed. |
| 5 | FROM paas.local chéo tenant | Ép `image-resolve-mode=pull` cho Dockerfile user (thay vet text). Cần thử trên node build thật. |
| + | Phát hiện thêm: `# syntax=` | Từ chối `# syntax=` khác `docker/dockerfile:*` trong Dockerfile user. Không có thì 4 và 5 bị vượt qua. |

## Việc 1 — deploy.yml

Hiện tại: root mặc định, `DEPLOY_PASSWORD` qua sshpass, `StrictHostKeyChecking=accept-new` (MITM lần kết nối đầu), IP 202.92.6.129:24700 viết cứng ở `inputs.default` và `env`, `workflow_dispatch` cho đổi host/port/user.

Việc làm:
- Xoá inputs host/port/user (mâu thuẫn với host key đã ghim). Đưa `PAAS_DEPLOY_HOST`, `PAAS_DEPLOY_PORT`, `PAAS_DEPLOY_USER` vào vars. `concurrency.group` dùng hằng.
- `DEPLOY_SSH_KEY` bắt buộc. `DEPLOY_KNOWN_HOSTS` ghi vào `~/.ssh/known_hosts`, `StrictHostKeyChecking=yes`, bỏ `apt install sshpass`.
- `environment: production` để secret chỉ lộ cho job chạy từ nhánh `deploy`. Bảo vệ nhánh `deploy`.
- Upload vào `~deploy/stage/` (không dùng `/tmp` world-writable, có race trên path đoán được), rồi `sudo /usr/local/sbin/paas-deploy`.
- Wrapper `paas-deploy` (root:root 0755): không nhận đường dẫn từ SSH, chỉ đọc từ `~deploy/stage/` cố định, copy sang thư mục root-owned rồi mới chạy. Tham số `--domain/--email/--dashboard-domain/--https` qua validate regex chặt. KHÔNG cho `--binary` tuỳ ý (nhận URL/đường dẫn = chạy file bất kỳ bằng root).
- sudoers: `deploy ALL=(root) NOPASSWD: /usr/local/sbin/paas-deploy`. Không wildcard.

Thứ tự trên server (đừng đảo):
1. Tạo user, authorized_keys, wrapper, sudoers (`visudo -c`).
2. Đổi workflow, chạy thử một deploy bằng user mới.
3. `PasswordAuthentication no`, `PermitRootLogin no`, reload sshd, giữ phiên root cũ mở để thử lại.
4. Đổi mật khẩu root. Xoá secret `DEPLOY_PASSWORD`. Coi mật khẩu cũ đã lộ (từng nằm trong GitHub secret).

KHÔNG làm: ký/kiểm chữ ký binary (workstream riêng); `from=` trong authorized_keys (dải IP runner GitHub quá rộng); đổi tên host khỏi lịch sử git (IP vẫn nằm trong history, không thu hồi được).

Cờ trung thực: deploy key vẫn ≈ root vì root chạy `paasd` do deploy user upload. Phần thắng thật: hết root login và mật khẩu, host key ghim, có audit riêng, hết race `/tmp`. Phần chưa thắng: containment khi lộ key.

## Việc 2 — sandboxed

`runtime.go:364-368`. `runtimeFor` trả `(string, any)`, một caller duy nhất (`createContainer`, :414). Đổi thành thêm `error`, caller trả lỗi ra. Container restart sau reboot cũng đi qua đây nên fail-closed đúng ý. `Capabilities()` tính lại mỗi lần heartbeat (agent.go:208), nên "capability cũ" không tồn tại lâu.

Tài liệu: README dòng 6 và PLAN.md ghi rõ `standard` = namespaces + seccomp + drop cap, root trong container, không userns; kernel/runc CVE là thoát host. Tenant không tin cậy → `sandboxed`.

KHÔNG làm: userns mặc định. Cần idmapped mount, chown volume (`withVolumeOwner`), kernel ≥5.12, kiểm CNI + snapshotter. Quá lớn cho nhóm I.

## Việc 3 — TLS DB public

- **postgres** (`tcp.go`, nhánh "A plain startup message"): hiện forward plaintext. Đổi thành `return`. Ngoại lệ: CancelRequest (code 80877102, length 16) vì libpq cũ gửi trên kết nối mới không TLS; nó chỉ mang pid + secret key, không mang credential. Không cho thì client cũ không huỷ được query qua port public.
- **mode `tls`** (redis, mongo): `tlsCfg == nil` hiện cho plaintext lọt qua. Đổi thành từ chối.
- **Không bật TLS ở edge**: `tcpManager.apply` bỏ qua route (log Warn), và API bật public port (`dbaas.go:160-199`) trả 409 "cần PAAS_TLS". `externalURL` bỏ nhánh `sslmode=disable`, luôn `require`. Lưu ý `tlsAtEdge()` đọc `cfg.PublicURL` của control plane, không phải trạng thái từng edge: edge đa node có thể lệch. Chấp nhận, ghi chú.
- **MySQL/MariaDB (`plain`)**: từ chối bật public; `tcpRoutes` bỏ route `plain`. DB MySQL đang public bị đóng. Cần: (a) thông báo/log lúc nâng cấp, (b) trả `public_port` về 0 hay giữ DB nhưng im port, chọn khi lập plan.

KHÔNG làm: STARTTLS MySQL ở edge (viết lại giao thức); `require_secure_transport` (phải cấp cert cho server, MariaDB không tự sinh); `verify-full` (DBHost có thể là IP, không khớp cert).

## Việc 4 — cache mount

Biết sẵn từ `brainstorm-261016-build-sandbox.md:40`: id do user chọn vẫn nhảy sang project khác nếu biết UUID. Rewrite trước khi gửi BuildKit.

Thiết kế:
- `vetDockerfile(path, scope) error` trong `internal/build`, chỉ chạy khi `plan.UserDockerfile != ""`. Không có BuildKit parser trong go.mod, dùng quét thủ công: nối dòng tiếp (`\`), nhận `RUN`, tách cờ `--mount=`.
- Mỗi `--mount=` có `type=cache`: có `id=X` → `id=<scope>/X`; không có → `id=<scope><target>` (target từ `target=`/`dst=`/`destination=`).
- Fail-closed: `type=` không phải literal (`type=$T`), hoặc số `type=cache` đếm trên toàn file ≠ số đã viết lại, hoặc `# escape=` khác `\` → từ chối Dockerfile với lỗi rõ. Không đoán.
- Helper `Options.cache` trong `detect.go` giữ nguyên cho Dockerfile sinh ra.

KHÔNG làm: BuildKit riêng mỗi project (RAM, cache nguội); cấm hẳn cache mount (hỏng Go/npm/pip của user).

## Việc 5 — FROM paas.local/

`build.go:314` đặt tên `paas.local/<serviceID>:<deploymentID>`; ID là UUID ngẫu nhiên, chỉ lộ trong team → rủi ro thực tế thấp (ex-member, ID lộ qua screenshot/ticket). Vet text bị vượt (`FROM ${A}/${B}` ghép từ build arg, `COPY --from=`, `--mount=from=`). Chốt: truyền `--opt image-resolve-mode=pull` cho Dockerfile user (`build.go` quanh :135). BuildKit không dùng store local khi resolve, và `paas.local` không phải registry thật nên fail.

Phase verify bắt buộc (như các plan trước), trên node build thật:
1. Build `paas.local/x:1`; build tiếp `FROM paas.local/x:1`: mặc định chạy được, với `pull` phải fail.
2. Base image thật (node, golang) vẫn build được; đo thêm độ trễ HEAD manifest mỗi build và rủi ro rate limit registry.
3. Nếu `pull` không chặn được → fallback vet text kèm quét `paas.local` toàn file và giá trị ARG, ghi rõ đó là speed bump.

Phát hiện thêm: `# syntax=` ở đầu Dockerfile user làm `dockerfile.v0` chuyển tiếp sang image frontend tuỳ ý. Frontend đó bỏ qua rewrite ở việc 4 và vet ở việc 5, đọc được cache và secret `paasenv` của build (là env của chính project đó). Từ chối `# syntax=` nếu không khớp `docker/dockerfile(:|@)`. Cùng hàm `vetDockerfile`.

## Rủi ro
- Wrapper deploy tự là code root: giữ ngắn, validate chặt, test bằng bats hoặc shell test với đầu vào xấu.
- `pull` có thể làm build chậm và chạm rate limit Docker Hub; có nguy cơ phải rollback sang vet text.
- Chặn MySQL public phá DB đang dùng: cần đường báo trước.
- Rewrite cache có thể từ chối Dockerfile hợp lệ nhưng lạ: thông báo lỗi phải nói rõ dòng nào và vì sao.

## Tiêu chí thành công
- Deploy chạy bằng user `deploy`, `ssh root@...` và đăng nhập mật khẩu đều bị từ chối.
- Máy không gVisor + service sandboxed → deploy fail với thông báo rõ, không có container runc nào được tạo.
- `psql "sslmode=disable"` vào port public bị từ chối; `sslmode=require` vào được; CancelRequest vẫn chạy.
- Mọi `type=cache` trong Dockerfile user tới BuildKit đều mang `id=<project>...` (test golden).
- `FROM paas.local/<service-khác>` fail trên node build thật.

## Thứ tự đề xuất
2 (nhỏ, độc lập) → 3 → 4+5+syntax (một hàm vet, phase verify trên node build) → 1 (cần quyền trên server, làm song song được; nếu mật khẩu root chưa đổi thì làm 1 trước).
