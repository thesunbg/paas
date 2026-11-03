# paas

[English](README.md) · **Tiếng Việt**

PaaS tự host, nhanh và nhẹ, tương tự Coolify, Heroku và Vercel. Push code lên GitHub là ứng dụng chạy trên domain riêng có HTTPS sau vài giây. Hỗ trợ frontend (site tĩnh, React, Vue, Next.js…), backend (Node.js, Go, Python, PHP, Ruby, Rust, Java, Deno) và database được quản lý sẵn (PostgreSQL, MySQL/MariaDB, Redis, MongoDB). Thêm máy chủ vào cụm chỉ bằng một lệnh.

- **Một file chạy duy nhất** (`paasd`, khoảng 37 MB) gồm control plane, agent trên node, edge proxy và NATS bus nhúng sẵn. Chạy tất cả trên một node chỉ tốn khoảng 40 MB RAM.
- **Không cần Docker daemon.** Container chạy trên containerd với crun hoặc runc; workload cần cô lập mạnh chạy bằng gVisor. Chế độ `standard` (namespace, seccomp, bỏ bớt capability, root bên trong container, không có user namespace) **không** đủ an toàn cho code không tin cậy: một lỗ hổng kernel hay runc có thể thoát ra host. Chỉ chế độ `sandboxed` (gVisor) là ranh giới an toàn, và node không có gVisor sẽ từ chối chạy service `sandboxed`.
- **Nhanh.** Build bằng BuildKit, có cache layer và cache mount. Site tĩnh được proxy phục vụ trực tiếp, không cần container. Deploy không downtime. Số đo trong môi trường dev:
  - deploy lại app Go đã có cache: khoảng 2,4 giây;
  - site tĩnh lên mạng: khoảng 0,9 giây;
  - app đang ngủ thức dậy: khoảng 1,7 giây.
- **Mật độ cao.** Các tính năng sau giúp một node chạy được nhiều app:
  - dùng chung server database;
  - lớp QoS theo request/limit, overcommit có kiểm soát;
  - chế độ ngủ, tự thức dậy khi có request;
  - tinh chỉnh runtime theo ngôn ngữ (heap và số worker tính theo giới hạn RAM);
  - đẩy bớt app khi thiếu RAM;
  - gợi ý chỉnh kích thước;
  - zram.
- **Chạy theo cụm.** Các node nối với nhau qua mạng WireGuard mã hoá, mỗi node có dải địa chỉ /16 riêng. Project được cách ly với nhau bằng nftables, kể cả khi nằm trên các node khác nhau. Registry nội bộ phân phối image tới node cần dùng.

Thiết kế và lộ trình: [docs/PLAN.md](docs/PLAN.md). REST API: [docs/API.md](docs/API.md).

## Tính năng

| Mảng | Bạn có gì |
|---|---|
| Deploy | Repo Git (webhook GitHub, tự deploy, commit status), Docker image, import Docker Compose, template một click (WordPress, n8n, Uptime Kuma, Umami, Gitea, Metabase…) |
| Git host | Repo private từ GitLab (gitlab.com hoặc tự host), Gitea / Forgejo (codeberg.org hoặc tự host) và Bitbucket Cloud, dùng token theo team (mã hoá, chỉ gửi tới đúng host của nó); webhook push-to-deploy có xác thực token / HMAC, preview cho MR/PR, commit status; lệnh `paas git-credentials`, `paas webhook` |
| Build | Tự nhận diện Node (npm/pnpm/yarn/bun, Next/Nuxt/SvelteKit/Remix/Astro/Vite/CRA/Angular/Nest/Express), Go, Python (Django/FastAPI/Flask), PHP (Laravel qua FrankenPHP), Ruby (Rails), Rust, Java (Maven/Gradle), Deno và HTML tĩnh. Dockerfile của bạn luôn được ưu tiên. Secret lúc build không bao giờ nằm trong layer của image. Image được build hoặc pull về đều được quét lỗ hổng (Trivy) |
| Chạy | Web service, worker, site tĩnh (SPA fallback, gzip, cache bất biến cho asset), cron job, preview cho từng PR, volume lưu trữ lâu dài |
| Vận hành | Deploy cuốn chiếu không downtime, rollback tức thì, biến môi trường mã hoá khi lưu, domain riêng có HTTPS tự động (Let's Encrypt), log build và log runtime trực tiếp, console web (exec), scale, autoscale (theo CPU / số request mỗi giây), lịch sử metrics, cảnh báo theo ngưỡng (CPU, RAM, số lần restart, tỉ lệ lỗi 5xx, số instance) có thời gian "for" và cooldown, uptime check HTTP(S) với tỉ lệ uptime 24 giờ / 7 ngày và cảnh báo chứng chỉ TLS sắp hết hạn, xuất metrics cho Prometheus qua `/metrics`, tự khởi động lại khi crash và tự phục hồi |
| Database | PostgreSQL / MySQL / MariaDB dùng chung hoặc riêng, cùng Redis và MongoDB; liên kết với service sẽ tự tiêm `DATABASE_URL`; backup định kỳ và khôi phục; bản sao off-site lên S3/R2/B2/MinIO; nâng cấp từ dùng chung lên riêng bằng một click |
| Mật độ | Lớp QoS (guaranteed / burstable / best-effort), overcommit theo node, chế độ ngủ, đẩy bớt app khi thiếu RAM (PSI), tinh chỉnh runtime theo ngôn ngữ, gợi ý kích thước, zram, dọn image và cache build, quota theo team (project, service, database, RAM và CPU đặt trước, dung lượng database, preview, domain riêng) |
| Cụm | `join` bằng một lệnh, mạng WireGuard, DNS nội bộ (`<service>.<project>.internal`), cách ly project giữa các node, registry image nội bộ, cordon / drain, theo dõi sức khoẻ node và xếp lại app, di chuyển service có volume giữa các node (copy qua mạng mesh, kiểm checksum, downtime ngắn; `drain --move-volumes`), khôi phục service có dữ liệu của node bị mất từ bản backup mới nhất, có ghi lại điểm khôi phục (RPO) |
| Bảo mật | Session + API token, xác thực hai lớp TOTP, đăng nhập một lần qua OpenID Connect (Google Workspace, Entra ID, Keycloak, Authentik…: PKCE, liên kết tài khoản an toàn, tự tạo tài khoản khi đăng nhập lần đầu, map group → vai trò trong team, bắt buộc SSO theo domain email nhưng owner vẫn giữ đăng nhập mật khẩu dự phòng), phân quyền theo team (owner/admin/developer/viewer), audit log, seccomp, no-new-privileges, giới hạn request theo IP ở edge, quét lỗ hổng image với chính sách theo project (off / warn / block theo ngưỡng mức độ) |
| Thông báo | Kênh webhook, Slack, Discord và Telegram cho: deploy, crash, node mất kết nối, backup và job thất bại, cảnh báo bật và tắt, uptime check down và phục hồi, chứng chỉ TLS sắp hết hạn |
| AI / MCP | Endpoint MCP (`/mcp`) để AI agent như Claude Code xem, deploy, rollback, restart và scale app bằng API token, theo đúng quyền của token đó; nút "Explain this failure" (giải thích lỗi) trên deployment thất bại (dùng Anthropic hoặc bất kỳ model tương thích OpenAI / tự host, secret được che, tắt mặc định) |
| Giao diện | Dashboard web (app Svelte nhúng sẵn, tiếng Anh và tiếng Việt: nút chuyển EN/VI, mặc định theo ngôn ngữ trình duyệt), CLI `paas`, REST API, MCP server cho AI agent ([docs/MCP.md](docs/MCP.md)) |

## Hiệu năng và dung lượng

Mục này giúp bạn ước lượng một server chạy được bao nhiêu app. Số liệu chia làm hai loại, ghi rõ ở từng bảng:
- **Đo thực tế**: đo trên môi trường dev (cụm 2 node, containerd + runc, BuildKit, PostgreSQL).
- **Ước tính**: tính từ cấu hình mặc định của paas (kích thước tài nguyên, overcommit, RAM dành cho hệ thống) và mức RAM điển hình của từng loại app. Con số thật phụ thuộc vào app và lượng truy cập; dashboard luôn hiện mức dùng thật để bạn chỉnh lại.

### Tốc độ (đo thực tế)

| Thao tác | Thời gian |
|---|---|
| Deploy lại app Go đã có cache build, tới lúc nhận traffic | khoảng 2,4 giây |
| Site tĩnh lên mạng | khoảng 0,9 giây |
| App đang ngủ thức dậy khi có request đầu tiên | khoảng 1,7 giây |
| Lỗi request trong lúc deploy cuốn chiếu | 0 |
| Leader control plane chết, bản khác lên thay (chạy HA) | khoảng 1 giây |

Vì sao nhanh:
- **Không có Docker daemon**: agent gọi thẳng containerd, container chạy bằng crun (viết bằng C, khởi động nhanh hơn runc khoảng 2 lần).
- **Build có cache nhiều tầng**: BuildKit giữ cache layer và cache mount (`node_modules`, Go module, pip…), nên lần build sau chỉ làm lại phần thay đổi.
- **Registry nội bộ**: image chỉ đi trong cụm, không đẩy lên Docker Hub rồi kéo về.
- **Site tĩnh không cần container**: proxy phục vụ thẳng file, có gzip và cache bất biến cho asset.
- **Đổi traffic không downtime**: bản mới chỉ nhận request khi đã qua health check, bản cũ được dừng sau.

### RAM của chính nền tảng

| Thành phần | RAM |
|---|---|
| `paasd` chạy đủ vai trò trên 1 node (control plane, agent, edge proxy, NATS) — đo thực tế | khoảng 40 MB |
| containerd | khoảng 20–30 MB |
| PostgreSQL lưu trạng thái control plane | khoảng 50–150 MB tuỳ số app |
| So với nền tảng dùng Docker daemon | tiết kiệm khoảng 100 MB mỗi node (không có `dockerd`) |

Mặc định mỗi node giữ lại **1 GB RAM** cho hệ điều hành và nền tảng; scheduler không xếp app vào phần này. Có thể chỉnh trong *Nodes → Settings* (`reserved_mb`).

### Kích thước app và cách scheduler tính

Mỗi app có một kích thước. **Request** là phần RAM scheduler giữ chỗ cho app; **limit** là mức tối đa app được dùng (vượt thì bị giới hạn, không lấn sang app khác).

| Kích thước | RAM request | RAM limit | CPU request | CPU limit |
|---|---|---|---|---|
| nano | 48 MB | 128 MB | 0,025 vCPU | 0,25 vCPU |
| micro | 96 MB | 256 MB | 0,05 vCPU | 0,5 vCPU |
| small | 160 MB | 512 MB | 0,1 vCPU | 1 vCPU |
| medium | 384 MB | 1 GB | 0,25 vCPU | 2 vCPU |
| large | 768 MB | 2 GB | 0,5 vCPU | 4 vCPU |
| xlarge | 1,5 GB | 4 GB | 1 vCPU | 8 vCPU |

Sức chứa của một node = (RAM − 1 GB dành cho hệ thống) × **1,5** (overcommit mặc định cho app loại burstable). Overcommit an toàn vì phần lớn app dùng ít hơn limit hầu hết thời gian; khi RAM thật sắp hết, agent đẩy bớt app ít quan trọng nhất và zram nén bộ nhớ để có thêm chỗ.

### Database: dùng chung tiết kiệm bao nhiêu (ước tính)

| Cách chạy | RAM |
|---|---|
| Mỗi app một PostgreSQL riêng | khoảng 100–250 MB mỗi app |
| Database logic trên server PostgreSQL dùng chung (mặc định của template) | khoảng 5–20 MB thêm mỗi app, cộng một server dùng chung khoảng 150–300 MB |
| Redis | khoảng 10–50 MB |

Ví dụ 30 app n8n: 30 PostgreSQL riêng tốn khoảng 3–7,5 GB; 30 database trên một server dùng chung chỉ khoảng 0,3–0,9 GB.

### Ví dụ: một server chạy được bao nhiêu n8n?

Template n8n: kích thước **small** (request 160 MB, limit 512 MB), database PostgreSQL **dùng chung**. Khi nhàn rỗi, một n8n thường dùng khoảng 150–250 MB RAM thật (ước tính).

| RAM server | n8n chạy liên tục (ước tính) | Batch bật chế độ ngủ (mặc định) |
|---|---|---|
| 4 GB | khoảng 12–15 | khoảng 50–100 bản sao, khoảng 10–12 bản hoạt động cùng lúc |
| 8 GB | khoảng 30–35 | 200 bản sao (tối đa một batch), khoảng 25–30 bản hoạt động cùng lúc |
| 16 GB | khoảng 65–75 | vài trăm bản sao (nhiều batch), khoảng 65 bản hoạt động cùng lúc |
| 32 GB | khoảng 140–150 | vài trăm đến hơn nghìn bản sao, khoảng 140 bản hoạt động cùng lúc |

Cách hiểu bảng:
- **Chạy liên tục**: giới hạn bởi RAM thật (khoảng 200 MB mỗi n8n), không phải bởi scheduler. Ví dụ 8 GB: còn khoảng 7 GB cho app → khoảng 35 n8n; scheduler cho phép tới (8 − 1) × 1,5 / 0,16 ≈ 67, nhưng xếp quá RAM thật sẽ chậm.
- **Chế độ ngủ**: bản sao không ai dùng không chiếm RAM (chỉ còn database logic khoảng 10 MB và dữ liệu trên đĩa). Số bản sao khi đó bị giới hạn bởi dung lượng đĩa và số người dùng cùng lúc, phù hợp cho lớp học hay demo: 30 học viên nhưng chỉ khoảng 10 người mở cùng lúc thì chỉ 10 bản chiếm RAM. Bản sao thức dậy trong khoảng 2 giây khi có người mở.
- Khi tạo batch, paas tự tính trên chính server của bạn và hiện số bản có thể hoạt động cùng lúc; batch nào sẽ làm server hết RAM bị từ chối.

### Các loại app khác (ước tính, server 8 GB, chạy liên tục)

| App | Kích thước template | RAM thật thường gặp | Số app trên 8 GB |
|---|---|---|---|
| Site tĩnh (React, Vue build sẵn, HTML) | không cần container | 0 | hàng trăm đến hàng nghìn (chỉ giới hạn bởi đĩa) |
| API Go | small | 10–30 MB | 100+ (giới hạn bởi scheduler: khoảng 67 với small, khoảng 220 với nano) |
| API Node.js / Express | small | 60–120 MB | khoảng 55–65 (scheduler cho tối đa 67 app small) |
| API Python (FastAPI, Django) | small | 80–250 MB | khoảng 30–65 |
| WordPress (MariaDB dùng chung) | small | 80–150 MB | khoảng 45–65 |
| Next.js / Nuxt SSR | small | 150–300 MB | khoảng 25–45 |
| Uptime Kuma | micro | 80–120 MB | khoảng 60–85 |
| Metabase, Keycloak (Java) | medium | 400–700 MB | khoảng 10–15 |

Bật chế độ ngủ cho các app ít dùng (preview, side project, demo) thường tăng số app lên gấp 2–3 lần.

### Ổ đĩa

| Nội dung | Dung lượng thường gặp |
|---|---|
| Image của một app (giữ 3 bản gần nhất để rollback, chỉnh bằng `PAAS_IMAGE_RETENTION`) | khoảng 0,2–1 GB mỗi app |
| Database logic mặc định | hạn mức 1 GB mỗi database (chỉnh được) |
| Build cache | được dọn tự động |

Một server 8 GB RAM / 160 GB SSD thường hết RAM trước khi hết đĩa, trừ khi app lưu nhiều file (Nextcloud, Jellyfin…).

### Tự kiểm tra trên server của bạn

- *Nodes*: RAM đã giữ chỗ, RAM thật đang dùng và tỉ lệ overcommit của từng node.
- Trang service → *Metrics*: CPU, RAM, request/giây của từng app; *Settings* gợi ý giảm kích thước khi app dùng ít hơn nhiều so với limit.
- *Batches → New batch*: ước tính cho đúng số bản sao và template bạn chọn, trên chính server của bạn.
- Trên server:

```bash
ps -o rss=,comm= -C paasd | awk '{printf "%s: %.0f MB\n", $2, $1/1024}'
```

```bash
free -m
```

## Cài đặt

**1. Control plane + node đầu tiên** (Ubuntu/Debian hoặc Fedora/RHEL, chạy bằng root):

```bash
curl -fsSL https://raw.githubusercontent.com/thesunbg/paas/main/internal/installer/install.sh \
  | sudo sh -s -- server --domain apps.example.com --email you@example.com
```

Hoặc cài từ laptop mà không cần tự đăng nhập vào server. CLI `paas` kết nối qua SSH, xác nhận host key, kiểm tra máy, cảnh báo nếu DNS chưa trỏ về server, chạy đúng script trên rồi chờ dashboard lên:

```bash
paas install --ssh root@203.0.113.10 --domain apps.example.com --email you@example.com
```

Mặc định lệnh này xác thực bằng SSH agent; dùng `--identity ~/.ssh/key` hoặc `--password` nếu không có agent. User không phải root thì cần quyền sudo. Host key được chấp nhận khi đã có trong `~/.ssh/known_hosts`, khi khớp với `--fingerprint SHA256:...`, hoặc khi bạn xác nhận trực tiếp (`--yes` tin host key ở lần kết nối đầu tiên). Host key khác với known_hosts luôn bị từ chối. `--installer-url` dùng một file install.sh khác; các cờ đặt sau `--` được chuyển nguyên cho script.

Dùng `--https` thay cho `--email` nếu muốn HTTPS tự động mà không cần email. Trỏ DNS của `*.apps.example.com` và `paas.apps.example.com` về server. Nếu sau này đổi `--domain` và chạy lại script, các domain tự sinh của app và webhook của GitHub App sẽ chuyển sang domain mới.

Sau đó mở `https://paas.apps.example.com` và tạo tài khoản quản trị. Một wizard cài đặt sẽ hiện ra, kiểm tra lần lượt DNS, HTTPS, cổng mạng, Git, backup off-site, kênh thông báo và lần deploy đầu tiên, kèm hướng dẫn cho từng bước. Mở lại wizard ở *Settings → System → Setup checklist*. Khi đang SSH vào server, lệnh `paas doctor` in ra cùng checklist đó.

Script sẽ cài:
- containerd 2, crun, runc, các CNI plugin và BuildKit;
- PostgreSQL để lưu trạng thái của control plane;
- zram và tinh chỉnh kernel;
- systemd unit cho `paasd`.

**2. Thêm server khi tải tăng.** Copy lệnh join ở *Nodes → Add node* (hoặc chạy `paas join-command`):

```bash
curl -fsSL https://paas.apps.example.com/install.sh | sudo sh -s -- join \
  --url https://paas.apps.example.com --token <join-token> --roles worker
```

Hoặc để control plane tự làm: *Nodes → Add node → Install over SSH* (hoặc `paas node add root@203.0.113.11 --roles worker`). Nhập host, user SSH (root hoặc user có sudo), private key hoặc mật khẩu, rồi xác nhận fingerprint host key của server. Control plane sẽ:
1. kiểm tra máy: hệ điều hành, kiến trúc CPU, systemd, RAM, disk, cổng còn trống, đã có paasd chưa, có kết nối được tới control plane không;
2. chạy lệnh join ở trên với một token mới dùng một lần;
3. hiện trực tiếp log cài đặt và chờ tới khi node online.

Key hoặc mật khẩu chỉ dùng cho lần cài đó và **không bao giờ được lưu**. Các lần cài, checklist và log của chúng được giữ lại (`paas node installs`).

Token chỉ dùng được một lần, hết hạn nhanh, và chỉ được cấp khi `PAAS_PUBLIC_URL` là `https://`. Node đổi token lấy danh tính riêng của nó (`POST /api/v1/cluster/enroll`) và nhận địa chỉ bus cùng CA từ control plane.

Vai trò của node: `worker` (chạy app), `build` (chạy BuildKit) và `edge` (chạy proxy HTTPS). Các node phải kết nối được với nhau qua UDP 51820 (WireGuard) và tới control plane qua TCP 4222.

**Chuyển cụm cũ sang cơ chế mới** (node còn dùng token chung của cụm): đặt `PAAS_NATS_LEGACY_TOKEN=1` trên control plane, nâng cấp control plane, rồi chạy lệnh join mới trên từng node. Khi không còn node nào hiện `legacy` trong API nodes (`GET /nodes`), bỏ biến đó đi.

**3. Deploy:**

```bash
paas login --server https://paas.apps.example.com
paas projects create shop && paas use shop
paas create api --repo https://github.com/me/api --size small
paas db create main --engine postgres --link api      # tự tiêm DATABASE_URL
paas env set api LOG_LEVEL=info                       # restart cuốn chiếu
paas logs api -f
```

Sau đó thêm URL webhook hiện trên trang service vào repo GitHub (sự kiện `push`, thêm `pull_request` nếu muốn có preview). Từ đó mỗi lần push sẽ tự deploy.

## Kết nối GitHub

Vào **Settings → GitHub → Create GitHub App** (admin, chỉ làm một lần). GitHub tạo một app riêng cho server của bạn rồi đưa bạn quay lại dashboard. Tiếp theo bấm **Connect GitHub account**, chọn các repo cho phép truy cập. Khi tạo **New service**, chọn repo trong danh sách: ngôn ngữ, loại service và cổng được nhận diện tự động, và mỗi lần push hoặc mở pull request sẽ tự deploy mà không cần tự cấu hình webhook hay token.

`PAAS_PUBLIC_URL` của server phải truy cập được từ GitHub, và nếu là `https://` thì HTTPS phải hoạt động (xem [Xử lý sự cố](#xử-lý-sự-cố)).

### Đổi domain hoặc tạo lại GitHub App

GitHub App ghi cứng các URL quay về theo `PAAS_PUBLIC_URL` lúc tạo. Khi chuyển sang domain khác (ví dụ từ `paas.<ip>.sslip.io` sang `paas.tenmien.com`), hãy tạo app mới:

1. Kiểm tra `/etc/paas/paasd.env`: `PAAS_PUBLIC_URL`, `PAAS_DASHBOARD_DOMAIN` và `PAAS_BASE_DOMAIN` đã là domain mới. Cách chắc nhất là chạy lại `install.sh server --domain <domain mới> --email ...`.
2. Trên dashboard: *Settings → GitHub → Disconnect* app cũ. Thao tác này xoá luôn các liên kết tài khoản GitHub với app cũ.
3. Bấm **Create GitHub App**: GitHub tạo app mới với URL của domain mới.
4. Bấm **Connect GitHub account**, cài app mới vào tài khoản hoặc organization và chọn các repo.
5. Trên GitHub: *Settings → Developer settings → GitHub Apps* → app cũ → *Advanced* → **Delete GitHub App**.

Các service giữ nguyên repo; lần push tiếp theo sẽ deploy qua app mới, không cần tạo lại service.

## Database dạng dịch vụ

PostgreSQL, MySQL, MariaDB, Redis và MongoDB được quản lý sẵn, dùng chung hoặc riêng, gồm:
- truy cập từ bên ngoài qua `db.<base domain>:<port>` (có TLS và danh sách IP được phép; mở TCP 20000–20999 trên firewall);
- metrics sử dụng và quota dung lượng;
- connection pooling bằng PgBouncer;
- thêm user chỉ đọc hoặc đọc-ghi;
- đổi kích thước tại chỗ;
- khôi phục về một thời điểm (PITR);
- high availability với failover tự động cho PostgreSQL dùng riêng.

## High availability

Chạy nhiều control plane dùng chung một PostgreSQL (bản thân PostgreSQL này cũng cần HA):

```bash
# trên cp1 và cp2 (và cp3), trong /etc/paas/paasd.env
PAAS_SERVER_NAME=cp1
PAAS_DATABASE_URL=postgres://paas:…@db.internal:5432/paas
PAAS_MASTER_KEY=<cùng một key hex trên mọi control plane>
PAAS_CLUSTER_TOKEN=<cùng một token trên mọi control plane; chỉ dùng để xác thực kết nối giữa các control plane>
PAAS_NATS_CLUSTER_ADDR=0.0.0.0:6222
PAAS_NATS_ROUTES=nats://cp2:6222,nats://cp3:6222
```

Node nhận URL của bus và CA của cụm khi enroll. Trỏ hostname của dashboard về tất cả các control plane; NATS client tự biết các server còn lại qua cụm. Bus và kết nối giữa các control plane dùng TLS, với CA do các control plane tạo ra và chia sẻ qua database.

Control plane nào cũng phục vụ API và bus. Một leader được bầu ra (qua advisory lock của PostgreSQL) để chạy hàng đợi deploy, reconciler, cron, autoscaler và các tác vụ bảo trì. Khi leader chết, một control plane khác lên thay trong vài giây; các app đang chạy không bị ảnh hưởng.

Installer cài thêm một control plane bằng `server --database-url … --master-key-file … --server-name cp2 --routes nats://cp1:6222`. Backup, khôi phục trạng thái nền tảng và phục hồi khi mất node: xem [docs/DR.md](docs/DR.md).

## Nâng cấp

*Settings → System → Updates* hiện phiên bản của từng control plane và node, cùng bản release mới nhất. Leader kiểm tra bản mới mỗi 6 giờ; `PAAS_UPDATE_CHECK=false` để tắt, `PAAS_UPDATE_URL` để trỏ sang mirror. Bấm **Update** (hoặc `paas update --yes [--version vX.Y.Z]`, xem tiến độ bằng `paas update status`) để nâng cấp cuốn chiếu:

1. backup trạng thái nền tảng;
2. nâng cấp lần lượt từng control plane, leader sau cùng để quyền leader chuyển sang một control plane đã nâng cấp, và nó làm tiếp phần còn lại;
3. nâng cấp lần lượt từng node còn lại (worker, build, edge); node offline hoặc đang cordon được bỏ qua.

Mỗi máy:
- tải binary của bản release và kiểm tra với file `SHA256SUMS` của release. Thiếu hoặc sai checksum thì từ chối cài; `PAAS_UPDATE_ALLOW_UNVERIFIED=true` bỏ qua bước kiểm tra này và **không an toàn**;
- thay binary một cách nguyên tử, giữ bản cũ ở `/usr/local/bin/paasd.prev`;
- khởi động lại qua systemd. Container vẫn chạy bình thường.

Máy nào không lên lại với phiên bản mới trong 10 phút sẽ được trả về `paasd.prev` và quá trình nâng cấp dừng lại. Nếu binary mới không khởi động nổi, một timer systemd trên chính máy đó cũng tự trả về bản cũ. Có thể tạm dừng, tiếp tục hoặc huỷ quá trình nâng cấp, và rollback tay từng node.

Chỉ binary được rollback, database thì không. Migration chỉ thêm chứ không xoá, nên phiên bản trước vẫn chạy được với schema mới, và hạ phiên bản bị từ chối. Nếu release notes ghi một migration không thuận ngược, cách quay lại là bản backup trạng thái ở bước 1 (`paasd restore-state`). Bản build dev (bất cứ thứ gì không phải tag `vX.Y.Z`) không bao giờ được báo là có bản mới hơn.

> **Lần nâng cấp đầu tiên lên phiên bản có tính năng này phải làm tay**: chạy lại lệnh `install.sh server` với đúng các tham số cũ. Từ lần sau mới cập nhật được qua dashboard.

### Bảo vệ control plane khi hết RAM

`paasd` chạy với `OOMScoreAdjust=-900`. Từ phiên bản này, installer đặt cùng mức đó cho PostgreSQL cục bộ của control plane; các tiến trình con của nó được đặt lại về 0, nên một query chạy quá tay vẫn bị kill được và PostgreSQL tự phục hồi. Nhờ vậy, khi node hết RAM, kernel sẽ kill một app, không bao giờ kill nền tảng. Chạy lại install script để áp dụng cho server đang có, hoặc làm tay:

```bash
for u in postgresql.service postgresql@.service; do sudo mkdir -p /etc/systemd/system/$u.d; printf '[Service]\nOOMScoreAdjust=-900\nEnvironment=PG_OOM_ADJUST_FILE=/proc/self/oom_score_adj\nEnvironment=PG_OOM_ADJUST_VALUE=0\n' | sudo tee /etc/systemd/system/$u.d/paas-oom.conf >/dev/null; done
```

```bash
sudo systemctl daemon-reload && sudo systemctl restart postgresql paasd
```

Batch (cài một template cho cả lớp) khởi động 2 bản sao một lúc và cho mỗi bản ngủ ngay khi đã chạy xong. Nhờ vậy tạo 30 bản sao không còn là boot 30 app cùng lúc, và chỉ những bản đang được dùng mới chiếm RAM. Batch nào sẽ làm node hết RAM bị từ chối, kể cả khi bấm "Create anyway".

## Cấu hình

`paasd` được cấu hình bằng biến môi trường trong `/etc/paas/paasd.env`:

| Biến | Mặc định | Ý nghĩa |
|---|---|---|
| `PAAS_DATABASE_URL` | `postgres://paas:paas@127.0.0.1:5432/paas` | nơi lưu trạng thái của control plane |
| `PAAS_BASE_DOMAIN` | `localhost` | app có domain `<service>-<project>.<base>` |
| `PAAS_DASHBOARD_DOMAIN` | `paas.<base>` | hostname của dashboard trên edge proxy |
| `PAAS_PUBLIC_URL` | `http://paas.<base>` | dùng trong URL webhook và các đường link |
| `PAAS_ACME_EMAIL` | | bật HTTPS tự động |
| `PAAS_DATA_DIR` | `/var/lib/paas` | volume, build, backup, key |
| `PAAS_HTTP_ADDR` / `PAAS_NATS_ADDR` | `:8080` / `0.0.0.0:4222` | API và bus của cụm |
| `PAAS_PROXY_HTTP_ADDR` / `PAAS_PROXY_HTTPS_ADDR` | `:80` / `:443` | edge proxy |
| `PAAS_PROXY_RATE_LIMIT` / `PAAS_PROXY_RATE_BURST` | `0` (tắt) | số request mỗi giây cho mỗi IP client |
| `PAAS_ROLES` | `worker,build` | vai trò của node |
| `PAAS_OCI_RUNTIME` | `crun` | `crun` hoặc `runc` |
| `PAAS_CLUSTER_BASE` | `10.100` | dải địa chỉ của container (node thứ *n* nhận `10.(100+n).0.0/16`) |
| `PAAS_WG_PORT` / `PAAS_MESH` | `51820` / `true` | mạng WireGuard |
| `PAAS_IMAGE_RETENTION` | `3` | số deployment giữ lại cho mỗi service để rollback |
| `PAAS_PUBLIC_IPS` | địa chỉ của các node | IP public mà DNS cần trỏ về (dùng cho setup checklist), cách nhau bằng dấu phẩy; cần đặt khi server nằm sau NAT |
| `PAAS_METRICS_TOKEN` | | bearer token cho endpoint Prometheus `GET /metrics` (admin cũng dùng được API token của mình) |
| `PAAS_UPTIME_ALLOW_PRIVATE` | `false` | cho uptime check truy cập địa chỉ private (chỉ dùng trong mạng tin cậy) |
| `PAAS_S3_ENDPOINT`, `PAAS_S3_BUCKET`, `PAAS_S3_ACCESS_KEY`, `PAAS_S3_SECRET_KEY`, `PAAS_S3_REGION`, `PAAS_S3_PREFIX` | | backup database off-site |
| `PAAS_EVICT_PSI` / `PAAS_EVICT_MIN_FREE_MB` | `40` / `256` | ngưỡng đẩy bớt app khi thiếu RAM |
| `PAAS_UPDATE_URL` | bản release mới nhất của thesunbg/paas trên GitHub | endpoint release để kiểm tra bản mới (JSON của GitHub API) |
| `PAAS_UPDATE_CHECK` | `true` | `false` để tắt kiểm tra bản mới mỗi 6 giờ |
| `PAAS_UPDATE_ALLOW_UNVERIFIED` | `false` | **không an toàn**: cài binary release không có checksum SHA-256 |
| `PAAS_SERVER_NAME`, `PAAS_NATS_CLUSTER_ADDR`, `PAAS_NATS_ROUTES`, `PAAS_NATS_ADVERTISE`, `PAAS_MASTER_KEY`, `PAAS_CLUSTER_TOKEN` | | high availability (xem ở trên) |

## Xử lý sự cố

Chạy `paas doctor` trên server hoặc mở *Settings → System → Setup checklist* trước tiên: checklist kiểm tra DNS, HTTPS, cổng và các phần còn lại, kèm hướng dẫn sửa.

**Trình duyệt báo `ERR_SSL_PROTOCOL_ERROR` khi mở dashboard, hoặc khi GitHub đưa bạn quay lại sau bước cài GitHub App.** HTTPS đang bật nhưng proxy không lấy được chứng chỉ cho hostname đó. Proxy xin chứng chỉ Let's Encrypt ngay lần kết nối đầu tiên; nếu xin không được thì kết nối HTTPS bị đóng. Xem log:

```bash
journalctl -u paasd --since "2 hours ago" --no-pager | grep -i -E "acme|certificate|obtain|rate|challenge" | tail -30
```

| Log báo | Nguyên nhân | Cách xử lý |
|---|---|---|
| `too many certificates`, `rateLimited` | Chạm giới hạn của Let's Encrypt. Hay gặp với domain dùng chung như `sslip.io` hoặc `nip.io`, vì rất nhiều người cùng xin chứng chỉ cho chúng | Dùng domain riêng: trỏ `*.apps.tenban.com` về IP server rồi chạy lại `install.sh server --domain apps.tenban.com --email ...`; hoặc đợi giới hạn hết hạn |
| `timeout`, `connection`, `challenge failed` | Let's Encrypt không vào được cổng 80 | Mở TCP 80 và 443 trên firewall của server và trong panel của nhà cung cấp VPS |
| `unknown host` | Hostname không khớp `PAAS_DASHBOARD_DOMAIN` / domain của app | Sửa `/etc/paas/paasd.env` cho đúng rồi `systemctl restart paasd` |

Nếu bước quay về sau khi cài GitHub App bị lỗi thì app vẫn đã được cài phía GitHub. Khi HTTPS chạy lại, vào *Settings → GitHub* và kết nối lại; không cần tạo lại app.

**Tạo quá nhiều app, server hết RAM, app hoặc dashboard bị crash.**

```bash
free -m
```

```bash
dmesg -T | grep -i -E "oom|killed process" | tail -20
```

```bash
systemctl status paasd postgresql --no-pager
```

Nếu `paasd` hoặc PostgreSQL bị kill, chạy `systemctl restart postgresql paasd`, rồi vào dashboard xoá batch hoặc dừng bớt service (`paas stop <service>`). Bật bảo vệ OOM cho PostgreSQL như ở mục [Bảo vệ control plane khi hết RAM](#bảo-vệ-control-plane-khi-hết-ram), và bật chế độ ngủ cho các app ít dùng.

## Phát triển

```bash
make build            # bin/paasd và bin/paas
make web              # build lại dashboard vào internal/webui/dist
make test             # unit test
make test-integration # cần root, containerd, buildkitd (containerd worker) và các CNI plugin
```

Test cần database chỉ chạy khi có `PAAS_TEST_DATABASE_URL`, ví dụ:

```bash
export PAAS_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
```

Để chạy trên máy local cần containerd, BuildKit (containerd worker, namespace `paas`), các CNI plugin trong `/opt/cni/bin` và PostgreSQL. Khởi động bằng `PAAS_ALLOW_LOCAL_GIT=true sudo -E bin/paasd all`, rồi mở http://127.0.0.1:8080. Thư mục `examples/` có sẵn vài app nhỏ bằng Node, Go, Python và site tĩnh để thử.

Cấu trúc thư mục:

```
cmd/paasd        server | agent | proxy | all
cmd/paas         CLI
internal/server  REST API, pipeline deploy, reconciler, kết nối scheduler, database, preview, job, autoscale
internal/agent   agent trên node (lệnh, heartbeat, đẩy bớt app, phục vụ file tĩnh)
internal/runtime runtime containerd (crun/runc/gVisor), log, exec, metrics
internal/network bridge CNI, mạng WireGuard, cách ly nftables, DNS nội bộ
internal/build   pipeline git + BuildKit;  internal/detect  nhận diện ngôn ngữ
internal/proxy   edge proxy (định tuyến, TLS, file tĩnh, đánh thức app, giới hạn request)
internal/...     store (PostgreSQL), scheduler, resources (QoS/tinh chỉnh), databases, templates, cron, s3, totp, secrets
web/             mã nguồn dashboard (Svelte 5)
```
