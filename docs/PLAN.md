# Kế hoạch xây dựng nền tảng PaaS

> Mục tiêu: một PaaS tự host giống Coolify / Heroku / Vercel. Người dùng kết nối GitHub, bấm deploy, và có ứng dụng chạy trên domain có HTTPS trong vài chục giây. Hỗ trợ frontend (HTML tĩnh, React, Vue, Next.js…), backend (Node.js, Go, Python, PHP, Java, Rust…) và database (PostgreSQL, MySQL, Redis, MongoDB…). Chạy theo cluster, thêm máy chủ mới bằng một lệnh khi tải tăng.
>
> Nguyên tắc thiết kế: **nhanh, nhẹ, đơn giản trước, mở rộng sau.**

---

## 1. Nguyên tắc kiến trúc

| Nguyên tắc | Cụ thể |
|---|---|
| **Nhẹ** | Mọi thành phần hệ thống viết bằng Go và đóng gói thành một binary tĩnh. Không bắt buộc Kubernetes, **không dùng Docker daemon**: agent điều khiển thẳng containerd + crun. Agent trên mỗi node dùng dưới 50 MB RAM. |
| **Nhanh** | Build có cache nhiều tầng. Site tĩnh không cần container. Registry nằm trong cluster. Deploy không downtime. Rollback tức thì vì giữ lại image cũ. |
| **Mật độ cao** | Nhồi được nhiều app nhất trên mỗi máy: dùng chung database server, overcommit có kiểm soát, cho app nhàn rỗi ngủ, tự tinh chỉnh runtime theo giới hạn RAM (xem mục 4.9). |
| **Cluster từ ngày đầu** | Kể cả khi chỉ có 1 máy, mọi thao tác đều đi qua *Agent API*, nên thêm node mới không phải viết lại gì. |
| **Control plane không giữ state** | State nằm trong PostgreSQL và NATS, nên chạy nhiều bản control plane để HA rất dễ. |
| **Mặc định an toàn** | Có mTLS giữa các node, mã hoá secret, giới hạn tài nguyên cho mọi container, và cô lập mạng giữa các project. |

---

## 2. Kiến trúc tổng quan

```
                         ┌──────────────────────────────┐
   GitHub ──webhook──▶   │        CONTROL PLANE         │  ◀── Dashboard (Web UI)
                         │  (Go, stateless, N bản sao)  │  ◀── CLI / REST API
                         │                              │
                         │  API · Auth · Scheduler      │
                         │  Build orchestrator          │
                         │  Reconciler · GitHub App     │
                         └──────┬──────────────┬────────┘
                                │              │
                     PostgreSQL (state)   NATS JetStream (event bus, queue,
                                │          lệnh tới agent, stream log)
                                │              │
          ┌─────────────────────┼──────────────┼──────────────────────┐
          │           WireGuard mesh (mạng overlay mã hoá)            │
          │                                                           │
   ┌──────▼──────┐      ┌──────────────┐      ┌──────────────┐     ┌──────────────┐
   │ EDGE NODE   │      │ BUILD NODE   │      │ WORKER NODE  │ ... │ WORKER NODE  │
   │ Proxy (TLS) │      │ BuildKit     │      │ Agent        │     │ Agent        │
   │ Agent       │      │ Agent        │      │ containerd + │     │ containerd + │
   └─────────────┘      └──────────────┘      │ crun/gVisor  │     │ crun/gVisor  │
                                              │ App + DB     │     │ App + DB     │
                                              └──────────────┘     └──────────────┘
          Registry nội bộ (Zot) · Object storage (S3: Garage/SeaweedFS)
          Metrics (VictoriaMetrics) · Logs (VictoriaLogs)
```

**Vai trò node.** Một node có thể giữ một hoặc nhiều vai trò. Với 1 máy thì máy đó giữ tất cả vai trò.

- `control`: chạy API, scheduler, Postgres, NATS.
- `edge`: nhận traffic từ Internet, terminate TLS, route request tới app.
- `build`: build image, có thể tách riêng để build không làm chậm app đang chạy.
- `worker`: chạy app và database.

---

## 3. Lựa chọn công nghệ

### 3.1. Lõi hệ thống

| Thành phần | Lựa chọn | Lý do | Phương án khác |
|---|---|---|---|
| Ngôn ngữ backend | **Go** | Binary tĩnh, ít RAM, concurrency tốt. Hệ sinh thái container (containerd, BuildKit, runc, gVisor) đều viết bằng Go. | Rust: nhanh hơn nhưng phát triển chậm hơn |
| Container runtime | **containerd + crun** ngay từ MVP (không dùng Docker Engine) | Không có dockerd nên mỗi node tiết kiệm khoảng 100 MB RAM. crun viết bằng C, khởi động nhanh hơn runc khoảng 2 lần. Agent gọi containerd trực tiếp qua thư viện Go `containerd/client`. Vẫn chạy được mọi image OCI/Docker. Chi tiết ở mục 3.6. | Docker Engine (dễ debug hơn nhưng nặng hơn), Podman |
| Điều phối (orchestration) | **Scheduler + Agent tự viết** | Nhẹ hơn k8s rất nhiều và kiểm soát hoàn toàn tốc độ deploy. Scheduler chỉ cần bin-packing, affinity và placement cho volume. | k3s (khi cần hệ sinh thái k8s), Nomad (giấy phép BSL), Docker Swarm (ít được phát triển tiếp) |
| State store | **PostgreSQL 16+** | Đáng tin cậy, có transaction, LISTEN/NOTIFY. Dùng `sqlc` + `pgx`. | SQLite cho bản single-node "lite" |
| Message bus | **NATS JetStream** | Một binary Go khoảng 20 MB. Dùng cho pub/sub, hàng đợi build, request/reply tới agent, KV store và stream log realtime. | Redis Streams |
| Mạng giữa các node | **WireGuard** (dùng `wgctrl-go`, control plane tự cấp IP) | Nhanh, chạy trong kernel, có mã hoá, cấu hình đơn giản. | Tailscale/Headscale, Flannel |
| Service discovery | **DNS nội bộ trong agent** (`<service>.<project>.internal`) | Không cần thêm thành phần. Agent nhận bản đồ service qua NATS. | CoreDNS |
| Giao tiếp agent ↔ control | **gRPC + mTLS** (dùng `connectrpc`) cùng NATS | Có kiểu dữ liệu chặt, hỗ trợ streaming. Chứng chỉ do CA nội bộ cấp lúc node join. | |

### 3.2. Build & Deploy

| Thành phần | Lựa chọn | Lý do |
|---|---|---|
| Build engine | **BuildKit** (`buildkitd` chạy trên build node) | Build song song, cache mount, cache xuất ra registry, build rootless. |
| Tự nhận diện ngôn ngữ | **Railpack** (bản kế nhiệm Nixpacks của Railway), bổ sung **Dockerfile** và **Docker Compose** | Không cần Dockerfile. Image nhỏ, build nhanh, hỗ trợ Node, Python, Go, PHP, Ruby, Java, Rust, Deno, Bun, Elixir, site tĩnh… |
| Registry | **Zot** (OCI registry viết bằng Go, nhẹ) | Chạy trong cluster nên pull qua LAN hoặc WireGuard rất nhanh. Có garbage collection. |
| Object storage | **Garage** hoặc **SeaweedFS** (tương thích S3) | Chứa site tĩnh, artifact build và backup DB. Có thể dùng S3, R2 hoặc B2 bên ngoài. |
| Site tĩnh / SPA | Build xong thì **upload thẳng lên object storage** và proxy phục vụ trực tiếp, **không chạy container** | Không tốn RAM, không cold start, deploy là đổi con trỏ "current" nên diễn ra tức thì (cách Vercel làm). |

### 3.3. Edge / Proxy

| Thành phần | Lựa chọn | Lý do |
|---|---|---|
| Reverse proxy | **Caddy** (cấu hình qua Admin API) ở MVP, sau đó có thể thay bằng **proxy Go tự viết dùng `certmagic`** | Tự xin TLS từ Let's Encrypt/ZeroSSL, có on-demand TLS cho custom domain, HTTP/3. Tự viết proxy thì làm được scale-to-zero, routing site tĩnh và hàng đợi request khi app đang khởi động. |
| DNS | Wildcard `*.apps.example.com` trỏ tới các edge node. Tích hợp Cloudflare API để tự tạo record và xin wildcard cert qua DNS-01. | |
| Load balancing | Proxy cân bằng tải giữa các replica qua WireGuard, có health check chủ động và bị động. | |

### 3.4. Quan sát hệ thống (observability)

| Thành phần | Lựa chọn | Lý do |
|---|---|---|
| Metrics | Agent tự thu thập số liệu cgroup (CPU, RAM, mạng, IO) rồi đẩy vào **VictoriaMetrics** | Ít RAM hơn Prometheus nhiều. Không cần cAdvisor. |
| Logs | Agent đọc stdout/stderr của container. Log realtime đi qua **NATS**, log lưu trữ nằm ở **VictoriaLogs** | Nhẹ. Truy vấn và xem tail nhanh. |
| Uptime & alert | Control plane tự kiểm tra, gửi cảnh báo qua Email, Slack, Discord, Telegram hoặc webhook | |
| Tracing (tuỳ chọn) | Nhận OpenTelemetry (OTLP) | |

### 3.5. Giao diện

| Thành phần | Lựa chọn |
|---|---|
| Dashboard | **SvelteKit** (bundle nhỏ, nhanh), hoặc React + Vite + TanStack Query nếu team quen React. Dùng Tailwind + shadcn. Dữ liệu realtime qua SSE/WebSocket. |
| CLI | Go + Cobra: `paas login`, `paas deploy`, `paas logs -f`, `paas env set`, `paas db create`, `paas node join` |
| API | REST + OpenAPI (sinh SDK tự động), token theo scope |
| IaC (sau này) | Terraform provider, file `paas.toml` đặt trong repo |

### 3.6. Tầng runtime (chạy container không cần Docker)

Agent điều khiển trực tiếp containerd và tự làm những việc Docker vốn lo sẵn:

```
paas-agent (Go)
  ├── containerd/client ──▶ containerd ──▶ shim ──▶ crun   (mặc định)
  │                                          └──▶ runsc  (gVisor, code không tin cậy)
  │                                          └──▶ Firecracker (microVM, giai đoạn 4, cần KVM)
  ├── Snapshotter: overlayfs  → sau thêm stargz/SOCI (lazy pulling)
  ├── Mạng: CNI (bridge + host-local IPAM) + nftables do agent quản lý
  │         (không dùng docker-proxy; port mapping bằng DNAT trong kernel)
  ├── Log: đọc FIFO stdout/stderr từ shim → ring buffer → NATS / VictoriaLogs
  └── Metrics: đọc cgroup v2 trực tiếp (cpu.stat, memory.current, io.stat)
```

| Thành phần | Lựa chọn | Ghi chú |
|---|---|---|
| Daemon | **containerd 2.x** | Một daemon nhẹ khoảng 20–30 MB RAM. Kubernetes cũng dùng đúng daemon này. |
| OCI runtime mặc định | **crun** | Viết bằng C, khởi động nhanh, ít RAM, hỗ trợ cgroup v2 đầy đủ. |
| Runtime cô lập mạnh | **gVisor (`runsc`)** | Dùng cho app của người dùng ngoài (multi-tenant) trên VPS không có KVM. I/O chậm hơn khoảng 10–30%. |
| microVM (giai đoạn 4) | **Firecracker** (qua `firecracker-containerd` hoặc tự chuyển OCI image thành rootfs) | Boot khoảng 125 ms, mỗi VM tốn khoảng 5 MB RAM. Cần bare metal hoặc máy hỗ trợ nested virtualization. |
| Mạng container | **CNI plugins** (bridge, host-local, portmap) + **nftables** | Mỗi project một bridge/subnet riêng. nftables chặn traffic giữa các project và DNAT từ proxy vào container. |
| Image | **Lazy pulling** bằng eStargz/SOCI snapshotter (giai đoạn 2–3) | Container chạy trước khi tải xong image, giảm thời gian start với image lớn. |
| Edge functions (giai đoạn 4) | **WebAssembly** (Wasmtime/Spin qua `runwasi` shim của containerd) | Khởi động dưới 1 ms, phù hợp scale-to-zero. Không thay thế container cho app thường. |

**Chọn runtime theo từng service.** Trường `runtime_class` trong spec của service nhận các giá trị `standard` (crun), `sandboxed` (gVisor), `microvm` (Firecracker) hoặc `wasm`. Scheduler chỉ đặt service lên node có hỗ trợ runtime đó (node khai báo khả năng khi join, ví dụ `kvm=true`).

**Debug khi không có Docker CLI.** Dùng `nerdctl` (cú pháp giống Docker) và `ctr` để thao tác tay, cùng lệnh `paas exec` / `paas logs` của CLI.

**Môi trường dev.** Trên Linux, chạy containerd trực tiếp. Trên macOS/Windows, dùng Lima hoặc một VM Linux nhỏ.

---

## 4. Tính năng chi tiết

### 4.1. Quản lý người dùng & tổ chức
- Đăng nhập bằng email/mật khẩu hoặc GitHub OAuth, có 2FA (TOTP), sau này thêm SSO (OIDC).
- Cấu trúc **Team → Project → Environment (production / staging / preview) → Service**.
- RBAC với các vai trò Owner, Admin, Developer, Viewer.
- API token theo scope, ghi audit log mọi thao tác.
- Quota theo team (CPU, RAM, số app, dung lượng); về sau có thể tính phí.

### 4.2. Tích hợp GitHub
- **GitHub App** (không dùng PAT), cho phép người dùng chọn repo được truy cập.
- Nhận webhook `push` và tự deploy theo nhánh đã cấu hình.
- **Preview deployment cho mỗi Pull Request**, URL dạng `pr-123.app.example.com`. Hệ thống comment link vào PR và tự xoá khi PR đóng.
- Cập nhật Check Run và Commit Status (building / deployed / failed) kèm link log.
- Hỗ trợ monorepo: chọn root directory, chỉ deploy khi các path liên quan thay đổi (watch paths).
- Ở giai đoạn sau hỗ trợ thêm GitLab, Bitbucket, Gitea và Git URL công khai.

### 4.3. Loại service được hỗ trợ
| Loại | Cách chạy |
|---|---|
| **Site tĩnh / SPA** (HTML, React, Vue, Vite, Astro…) | Build ra thư mục `dist`, lưu lên S3, proxy phục vụ trực tiếp. Có SPA fallback, header, redirect/rewrite (`_redirects`), nén gzip/brotli và cache bất biến cho asset có hash. |
| **Web service** (Node, Go, Python, PHP, Java, Ruby, Rust, .NET…) | Chạy container, lắng nghe `$PORT`, có health check HTTP. |
| **SSR framework** (Next.js, Nuxt, SvelteKit, Remix) | Chạy như web service. Asset tĩnh `/_next/static` được tách ra S3 để phục vụ nhanh hơn. |
| **Worker / background job** | Container không có port public. |
| **Cron job** | Lịch chạy định kỳ theo cú pháp cron, mỗi lần chạy là một container ngắn hạn, lưu lịch sử. |
| **Docker image có sẵn** | Deploy thẳng từ Docker Hub, GHCR hoặc registry riêng. |
| **Docker Compose** | Import file compose thành nhiều service. |
| **Template 1-click** | WordPress, Ghost, n8n, Plausible, Umami, Minio, Uptime Kuma… |

### 4.4. Database được quản lý
- Hỗ trợ **PostgreSQL, MySQL/MariaDB, Redis/Valkey, MongoDB**, sau đó thêm ClickHouse, RabbitMQ, Meilisearch.
- Tạo bằng 1 click, tự sinh credential và tự gắn biến `DATABASE_URL` vào app trong cùng project.
- Có hai chế độ: **shared** (mặc định cho dev, staging và app nhỏ: nhiều database logic chạy trên 1 database server) và **dedicated** (mỗi database một instance riêng, dành cho production). Chi tiết ở mục 4.9.
- Volume persistent nằm ở ổ local (NVMe) của node, scheduler **ghim DB vào node chứa volume**.
- **Backup tự động** lên S3 theo lịch (pgBackRest/WAL-G cho Postgres, hỗ trợ PITR), restore bằng 1 click, có retention policy.
- Mặc định chỉ truy cập qua mạng nội bộ. Có thể bật public qua TCP proxy kèm TLS.
- Giai đoạn sau: HA Postgres (Patroni hoặc tự viết failover), read replica, connection pooling (PgBouncer).
- Có giao diện xem số liệu DB: kết nối, dung lượng, slow query.

### 4.5. Build
- Tự nhận diện ngôn ngữ (Railpack) hoặc dùng Dockerfile. Ghi đè được build command, start command và phiên bản runtime.
- **Cache nhiều tầng**: cache layer của BuildKit lưu trong registry nội bộ, cache mount cho `node_modules`, `~/.cache/pip`, `~/go/pkg/mod`, `~/.cargo`, cùng base image có sẵn trên build node.
- Build log stream realtime lên dashboard và CLI.
- Hàng đợi build có giới hạn số build đồng thời theo team và theo node. Khi có commit mới thì huỷ build cũ của cùng nhánh.
- Build secret không bị ghi vào image (`--secret` của BuildKit).
- Có thể dùng lại image đã build cho cùng commit SHA, bỏ qua build khi promote từ staging lên prod.

### 4.6. Deploy & vận hành
- **Zero-downtime rolling deploy**: start bản mới, chờ health check pass, chuyển traffic, drain kết nối cũ, rồi dừng bản cũ.
- **Rollback tức thì** về bất kỳ bản deploy nào trước đó, vì image vẫn còn trong registry.
- Biến môi trường và secret mã hoá AES-256-GCM với master key (về sau dùng OpenBao/Vault). Có *shared variables* dùng chung theo project hoặc environment.
- Custom domain kèm TLS tự động, redirect www, nhiều domain cho một service.
- Giới hạn tài nguyên (CPU, RAM) cho từng service, có instance size định sẵn.
- **Persistent volume** cho app cần lưu file.
- Truy cập web terminal (exec vào container) và xem log realtime hoặc log lịch sử có tìm kiếm.
- Restart policy, health check (HTTP/TCP/command), lệnh pre-deploy (ví dụ migration DB).

### 4.7. Cluster & mở rộng
- **Thêm node bằng 1 lệnh**:
  ```bash
  curl -sSL https://paas.example.com/install.sh | sh -s -- join --token <JOIN_TOKEN> --roles worker
  ```
  Script cài agent và runtime, sinh khoá WireGuard, nhận chứng chỉ mTLS, rồi đăng ký với control plane. Node sẵn sàng sau khoảng 1–2 phút.
- **Scheduler** đặt container theo tài nguyên còn trống (bin-packing hoặc spread), label/affinity (ví dụ `gpu=true`, `region=sg`), anti-affinity giữa các replica, và vị trí volume.
- **Drain / cordon node** để bảo trì: di chuyển workload stateless sang node khác.
- **Tự phục hồi**: reconciler liên tục so sánh trạng thái mong muốn với trạng thái thực tế. Khi node chết thì lên lịch lại replica stateless sang node khác.
- **Horizontal autoscaling** theo CPU, RAM hoặc RPS (đo ở proxy), có min/max replica.
- **Scale-to-zero (tuỳ chọn)** cho app ít truy cập: proxy giữ request lại trong lúc app khởi động (xem mục 4.9).
- **Tự động thêm và bớt node** qua API của nhà cung cấp cloud (Hetzner, DigitalOcean, Vultr, AWS) khi cluster thiếu hoặc thừa tài nguyên.
- **HA control plane**: 3 node control, gồm NATS cluster 3 node, Postgres primary + replica và nhiều bản control plane đặt sau load balancer.
- Đa vùng (multi-region) ở giai đoạn sau: edge node ở nhiều nơi, dùng GeoDNS.

### 4.8. Bảo mật
- Container chạy bằng crun, non-root với user namespace, seccomp/AppArmor mặc định, filesystem read-only khi có thể.
- **Cô lập mạng giữa các project** bằng nftables do agent quản lý: service khác project không thấy nhau.
- `runtime_class: sandboxed` (**gVisor**) cho code không tin cậy trên VPS thường. `runtime_class: microvm` (**Firecracker**) khi node có KVM.
- Giới hạn tốc độ request (rate limit) ở edge, bảo vệ dashboard bằng 2FA, quét image (Trivy) khi build.
- Secret không bao giờ xuất hiện trong log, bị che khi hiển thị trên UI.

### 4.9. Tối ưu mật độ (nhiều app hơn trên mỗi máy)

Trên một máy, RAM gần như luôn là giới hạn trước CPU và ổ đĩa. Các tính năng dưới đây giúp tăng số app chạy được trên cùng một máy lên khoảng 2–3 lần (ước tính ở mục 9).

**1. Database server dùng chung (shared database server)**
- Mỗi node (hoặc mỗi team) có 1 instance PostgreSQL và 1 instance MySQL dùng chung. Mỗi app nhận một **database logic riêng, user riêng và mật khẩu riêng**.
- Cô lập giữa các database:
  - Postgres: `REVOKE CONNECT` giữa các database, đặt `CONNECTION LIMIT` cho từng database, và theo dõi dung lượng từng database để cảnh báo khi vượt quota.
  - MySQL: phân quyền theo từng schema, đặt `MAX_USER_CONNECTIONS`.
- Có **PgBouncer** đứng trước Postgres dùng chung, để nhiều app chia sẻ ít kết nối thật.
- Backup và restore vẫn làm **theo từng database logic** (`pg_dump`/`mysqldump` từng database), cùng với backup toàn instance (pgBackRest/WAL-G).
- Bấm **"Nâng cấp lên dedicated"** để tách một database ra instance riêng. Hệ thống tự dump/restore hoặc dùng logical replication để ít downtime, rồi tự cập nhật `DATABASE_URL`.
- Mỗi database logic tiết kiệm khoảng 150–250 MB RAM so với chạy instance riêng.
- Redis vốn rất nhẹ (10–50 MB) nên vẫn chạy instance riêng cho mỗi app, giúp cô lập dễ hơn.

**2. Request / limit, QoS và overcommit có kiểm soát**
- Mỗi service có `memory.request` (lượng RAM được đảm bảo, scheduler dùng để xếp chỗ) và `memory.limit` (trần cứng, vượt là bị OOM kill). CPU cũng có cặp tương tự.
- Hệ thống tự gán **lớp QoS (quality of service)** cho từng service:

  | Lớp | Điều kiện | Áp dụng cho | Thứ tự bị dừng khi máy thiếu RAM |
  |---|---|---|---|
  | `guaranteed` | request = limit | Database, app production quan trọng | Dừng sau cùng |
  | `burstable` | request < limit | Mặc định cho app | Ở giữa |
  | `best-effort` | Không đặt request | Preview deployment, cron | Dừng đầu tiên |

- **Tỷ lệ overcommit chỉnh được theo node**, ví dụ RAM 1,5×, CPU 4×. Scheduler xếp app theo tổng `request` và không bao giờ để tổng `request` của app `guaranteed` vượt quá RAM thật.
- Agent cấu hình cgroup v2 như sau:
  - `memory.high` (ngưỡng mềm, kernel bắt đầu đòi lại RAM từ từ thay vì kill ngay) và `memory.max` (ngưỡng cứng).
  - `cpu.weight` để chia CPU theo tỷ lệ.
  - `oom_score_adj` theo lớp QoS, để kernel kill app ít quan trọng trước.

**3. Theo dõi áp lực bộ nhớ và chủ động đẩy app ra (eviction)**
- Agent đọc **PSI** (`/proc/pressure/memory`, chỉ số cho biết các tiến trình đang phải chờ RAM bao lâu) và mức RAM còn trống của node.
- Khi vượt ngưỡng, agent làm theo thứ tự:
  1. Cho app `best-effort` đang nhàn rỗi đi ngủ.
  2. Dừng các app `best-effort` còn lại.
  3. Báo scheduler chuyển app `burstable` (không có state) sang node khác.
- Mục tiêu là tránh để kernel OOM killer giết ngẫu nhiên.

**4. Chế độ ngủ (sleep mode) và scale-to-zero**
- App không có request trong N phút (mặc định 15 phút cho preview, tắt mặc định cho production) sẽ bị dừng và giải phóng toàn bộ RAM.
- **Giai đoạn 2, bản đơn giản**: proxy chuyển request của app đang ngủ sang dịch vụ "waker". Waker start lại container và trả về trang "đang khởi động" tự tải lại (hoặc HTTP 503 kèm `Retry-After` với API).
- **Giai đoạn 3, bản đầy đủ**: proxy Go tự viết **giữ request lại** trong lúc container start (thường dưới 1 giây với Go/Node vì crun và image đã có sẵn trên node), nên người dùng không thấy trang chờ.
- Hướng nghiên cứu (giai đoạn 4): dùng **CRIU checkpoint/restore** để "đánh thức" các app khởi động chậm (Java, Next.js) trong vài trăm ms thay vì vài giây.

**5. Tự tinh chỉnh runtime theo giới hạn RAM**
- Lúc khởi động, agent tự thêm các biến môi trường theo ngôn ngữ (Railpack đã nhận diện được) và theo `memory.limit`. Người dùng ghi đè được.

  | Ngôn ngữ | Biến môi trường tự thêm |
  |---|---|
  | Node.js | `NODE_OPTIONS=--max-old-space-size=<75% limit>` |
  | Go | `GOMEMLIMIT=<90% limit>`, `GOMAXPROCS=<số CPU được cấp>` |
  | Java | `JAVA_TOOL_OPTIONS=-XX:MaxRAMPercentage=75 -XX:+UseSerialGC -XX:TieredStopAtLevel=1` (mức nhẹ) |
  | Python | `WEB_CONCURRENCY` tính theo RAM/CPU được cấp (gunicorn/uvicorn) |
  | PHP | php-fpm `pm=ondemand` và `pm.max_children` tính theo RAM |

- Database dùng **preset theo kích thước**: `shared_buffers`, `max_connections`, `work_mem` của Postgres và `innodb_buffer_pool_size` của MySQL được tính theo RAM cấp, thay vì dùng cấu hình mặc định.

**6. Gợi ý chỉnh lại kích thước (right-sizing)**
- Agent ghi lại mức RAM và CPU thực dùng ở p50/p95/max trong 7 ngày.
- Dashboard đưa ra gợi ý như "App dùng tối đa 90 MB nhưng limit là 512 MB, giảm xuống 160 MB?", áp dụng bằng 1 click.
- Dashboard có trang **"Mật độ node"** hiển thị RAM đã cam kết, RAM thực dùng, tỷ lệ overcommit và số app đang ngủ.

**7. zram và swap**
- `install.sh` bật **zram** (swap nén ngay trong RAM, khoảng 25–50% dung lượng RAM, nén `zstd`) và đặt `vm.swappiness` phù hợp.
- Database `guaranteed` không được dùng swap (cgroup `memory.swap.max=0`), còn app `burstable`/`best-effort` thì được. Nhờ vậy máy chịu được lúc tải tăng đột biến mà không bị OOM.

**8. Tiết kiệm ổ đĩa và RAM cho build**
- **Garbage collection (GC) image**: mỗi service giữ N bản gần nhất để rollback (mặc định 3), xoá image không còn dùng trong containerd và Zot, giới hạn tổng dung lượng build cache (ví dụ 25 GB, xoá phần ít dùng nhất).
- **Base image dùng chung**: Railpack và preset build dùng cùng một bộ base image, nên các app chia sẻ layer với nhau. containerd tự khử trùng lặp layer giống nhau.
- **Giới hạn build**: `buildkitd` chạy trong cgroup riêng (ví dụ tối đa 3 GB RAM và 4 CPU), chỉ 1–2 build chạy cùng lúc trên máy đơn. Khi cluster có build node riêng thì worker không phải chừa RAM cho build.
- Log xoay vòng (log rotation) và giới hạn thời gian lưu metrics/log theo team.

**9. Không chạy container khi không cần**
- Site tĩnh và SPA phục vụ thẳng từ S3 (mục 4.3), không tốn RAM.
- Asset tĩnh của SSR (Next.js `/_next/static`…) tách ra S3, giảm tải cho container.
- Cron job chạy bằng container ngắn hạn, chạy xong là dừng.

---

## 5. Luồng deploy (từ git push đến URL)

```
1. git push ─▶ GitHub webhook ─▶ Control plane (kiểm tra chữ ký HMAC)
2. Tạo bản ghi Deployment (queued) ─▶ đưa job vào NATS "builds.<team>"
3. Build node nhận job:
     clone shallow (--depth 1) ─▶ Railpack/Dockerfile ─▶ BuildKit (có cache)
     ─▶ push image lên Zot nội bộ  (log stream qua NATS ─▶ UI)
   * Nếu là site tĩnh: upload dist/ lên S3 ─▶ đổi con trỏ ─▶ XONG
4. Scheduler chọn node ─▶ gửi lệnh "RunContainer" tới agent (gRPC)
5. Agent pull image qua LAN vào containerd ─▶ tạo container bằng crun
   ─▶ gắn mạng CNI + luật nftables ─▶ start ─▶ health check
6. Pass ─▶ Control plane cập nhật routing table ─▶ edge proxy nhận cấu hình mới
7. Drain và dừng container cũ ─▶ cập nhật GitHub Check Run ─▶ thông báo
```

**Mục tiêu tốc độ**

| Chỉ số | Mục tiêu |
|---|---|
| Deploy site tĩnh, đã có cache | < 15 giây |
| Deploy Node/Go/Python, đã có cache | < 30–45 giây |
| Rollback | < 3 giây (chỉ đổi routing nếu bản cũ còn chạy) / < 10 giây (start lại image cũ) |
| Start container (image đã có sẵn) | < 300 ms |
| Thêm node mới | < 2 phút |
| Độ trễ proxy cộng thêm | < 1 ms p50 |
| RAM của agent | < 50 MB (+ containerd khoảng 25 MB); control plane < 150 MB |

**Kỹ thuật để đạt tốc độ**
- Shallow clone, chỉ tải code của commit cần build.
- Cache BuildKit trong registry nội bộ, dùng chung giữa các build node.
- Giữ sẵn base image phổ biến trên build node và worker node.
- Không có Docker daemon: agent gọi thẳng containerd, crun khởi động container nhanh hơn runc.
- Port mapping bằng DNAT nftables trong kernel, không qua `docker-proxy`.
- Pull image qua mạng nội bộ. Giai đoạn 2–3 dùng *lazy pulling* (eStargz/SOCI) để container start trước khi pull xong.
- Image runtime nhỏ (distroless hoặc alpine, multi-stage build).
- Site tĩnh không cần container. Asset có hash được cache vĩnh viễn ở trình duyệt và CDN.
- Proxy có connection pooling và HTTP/2 tới upstream.

---

## 6. Mô hình dữ liệu (rút gọn)

```
users ─┬─< memberships >─ teams ─< projects ─< environments ─< services
       │                                                      │
       └─< api_tokens                                         ├─< deployments ─< build_logs (ref)
                                                              ├─< env_vars (encrypted)
nodes ─< node_resources                                       ├─< domains ─< certificates
  └─< instances (container đang chạy: service_id, node_id,    ├─< volumes (node_id)
       deployment_id, state, ip:port)                         └─< autoscale_policies
database_servers (shared/dedicated, node_id, engine, size_preset)
  └─< databases (database logic, owner service) ─< backups
git_installations ─< repositories
audit_logs, notifications, webhooks_in
```

- `services.spec` (JSONB) lưu **trạng thái mong muốn**: image, số replica, tài nguyên (`cpu/memory.request`, `cpu/memory.limit`), `qos_class`, `runtime_class`, chính sách ngủ (`sleep.idle_timeout`), port, health check.
- `nodes` lưu thêm tỷ lệ overcommit, dung lượng zram và các khả năng của node (ví dụ `kvm=true`).
- `usage_stats` (lưu trong VictoriaMetrics) chứa số liệu p50/p95 dùng cho gợi ý right-sizing.
- `instances` lưu **trạng thái thực tế** do agent báo về.
- Reconciler chạy liên tục để đưa trạng thái thực tế về trạng thái mong muốn.

---

## 7. Cấu trúc repo đề xuất (monorepo)

```
paas/
├── cmd/
│   ├── paas-server/      # control plane (API, scheduler, reconciler, GitHub)
│   ├── paas-agent/       # agent chạy trên mọi node
│   ├── paas-proxy/       # edge proxy (giai đoạn sau, thay Caddy)
│   └── paas/             # CLI
├── internal/
│   ├── api/              # HTTP handlers, OpenAPI
│   ├── auth/             # session, OAuth, RBAC, tokens
│   ├── scheduler/        # placement, bin-packing, overcommit theo QoS
│   ├── resources/        # request/limit, QoS, cgroup v2, PSI, eviction, tuning theo ngôn ngữ
│   ├── sleeper/          # chế độ ngủ, waker, scale-to-zero
│   ├── reconciler/       # desired vs actual state
│   ├── build/            # orchestrate BuildKit, Railpack
│   ├── runtime/          # interface Runtime: containerd (crun, gVisor), sau thêm firecracker, wasm
│   ├── network/          # CNI, nftables, WireGuard, DNS nội bộ
│   ├── proxy/            # routing table, Caddy client
│   ├── github/           # GitHub App, webhooks, checks
│   ├── database/         # template DB, backup/restore
│   ├── secrets/          # mã hoá env vars
│   ├── metrics/ logs/    # thu thập & truy vấn
│   └── store/            # Postgres (sqlc), migrations
├── proto/                # gRPC/Connect definitions (agent API)
├── web/                  # Dashboard (SvelteKit)
├── templates/            # 1-click templates (YAML)
├── deploy/               # install.sh, systemd units, docker-compose dev
└── docs/
```

---

## 8. Lộ trình triển khai

### Giai đoạn 0: Nền móng (2 tuần)
- Dựng monorepo Go, CI (lint, test), Postgres schema và migration (goose), sqlc.
- Khung API cùng auth cơ bản (email/password, session).
- Định nghĩa Agent API (proto) và interface `Runtime`.
- Script cài containerd + crun + CNI plugins lên node (dùng cho dev và cho `install.sh`).
- Môi trường dev: chạy Postgres, NATS, Zot, Caddy bằng `nerdctl compose` (hoặc process thường). Máy dev macOS/Windows dùng Lima.

### Giai đoạn 1: MVP single-node (7–10 tuần, thêm thời gian cho tầng runtime containerd)
- Agent điều khiển containerd + crun: pull, run, stop, exec, logs (đọc FIFO), stats (đọc cgroup v2).
- Mạng container bằng CNI bridge + nftables (port mapping, chặn traffic giữa các project).
- GitHub App: chọn repo, nhận webhook push, tự deploy.
- Build bằng BuildKit, Railpack và Dockerfile, push lên Zot, log realtime.
- Web service, worker và site tĩnh (qua S3).
- Caddy làm proxy, subdomain tự động và custom domain có TLS tự động.
- Biến môi trường mã hoá, zero-downtime deploy, rollback.
- PostgreSQL, MySQL và Redis 1-click, kèm backup lên S3. **Database server dùng chung** (Postgres/MySQL nhiều database logic, có PgBouncer) là chế độ mặc định, chọn được dedicated.
- **Mật độ cơ bản**: request/limit và lớp QoS ghi xuống cgroup v2, tự tinh chỉnh runtime theo ngôn ngữ, preset kích thước cho DB, zram trong `install.sh`, giới hạn build (cgroup cho buildkitd, 1–2 build đồng thời), GC image và build cache.
- Dashboard: project, service, deploy, log, env, domain. CLI cơ bản.
- **Kết quả**: thay thế được Coolify cho 1 máy chủ.

### Giai đoạn 2: Cluster (6–8 tuần)
- Lệnh `join` với token, WireGuard mesh, CA nội bộ, mTLS.
- Scheduler (bin-packing, affinity, volume pinning) và reconciler tự phục hồi.
- Nhiều replica, load balancing giữa các node, DNS nội bộ.
- Tách vai trò edge, build và worker. Drain/cordon node.
- Metrics (VictoriaMetrics) và logs (VictoriaLogs), biểu đồ trên dashboard, cảnh báo.
- Preview deployment cho PR, cập nhật GitHub Check Run.
- **Mật độ nâng cao**: scheduler overcommit theo QoS, agent theo dõi PSI và eviction, **chế độ ngủ bản đơn giản** (waker + trang "đang khởi động") cho preview và app nhàn rỗi, gợi ý right-sizing, trang "Mật độ node".
- Nâng cấp database shared lên dedicated bằng 1 click.

### Giai đoạn 3: Mở rộng & tối ưu (6–8 tuần)
- Horizontal autoscaling (CPU, RAM, RPS) và **scale-to-zero đầy đủ** (proxy giữ request trong lúc container start).
- Tự viết proxy Go (certmagic) để thay Caddy, routing nhanh hơn.
- Tự động thêm và bớt node qua API Hetzner/DO/AWS.
- HA control plane (3 node).
- Cron job, Docker Compose, template 1-click.
- Lazy pulling (stargz/SOCI), chuẩn hoá image cache.
- `runtime_class: sandboxed` bằng gVisor.

### Giai đoạn 4: Doanh nghiệp & multi-tenant (liên tục)
- RBAC chi tiết, SSO/OIDC, audit log, quota và tính phí (Stripe).
- `runtime_class: microvm` bằng Firecracker cho node có KVM.
- Edge functions bằng WebAssembly (runwasi).
- HA database (Patroni), read replica, PITR.
- Nghiên cứu CRIU checkpoint/restore để đánh thức nhanh app khởi động chậm. Bật KSM (gộp trang RAM giống nhau giữa các VM) cho node chạy microVM.
- Đa vùng, GeoDNS, CDN cho site tĩnh.
- Terraform provider, file cấu hình `paas.toml`, API công khai.

---

## 9. Ước tính dung lượng & phần cứng

### 9.1. Ví dụ: 1 máy 10 core / 10 GB RAM / 200 GB SSD, giữ tất cả vai trò

Các con số là ước tính cho app ít đến vừa traffic. RAM là giới hạn chính, còn CPU và ổ đĩa dư.

**Phần RAM hệ thống chiếm trước**

| Thành phần | RAM |
|---|---|
| Linux, containerd, agent | khoảng 0,5 GB |
| Control plane, Postgres (state), NATS | khoảng 0,4 GB |
| Caddy, Zot, Garage (S3), VictoriaMetrics/Logs | khoảng 0,5 GB |
| Chừa chỗ cho build (giới hạn bằng cgroup) | khoảng 2,5 GB |
| **Còn lại cho app** | **khoảng 6–6,5 GB** |

**RAM mỗi loại app (khi nhàn rỗi hoặc tải nhẹ)**

| Loại | RAM / app |
|---|---|
| Site tĩnh / SPA | 0 (proxy phục vụ thẳng từ S3) |
| Go | 10–30 MB |
| Node.js | 60–120 MB |
| Python | 80–250 MB |
| PHP Laravel | 60–150 MB |
| Next.js / Nuxt SSR | 150–300 MB |
| Java Spring Boot | 300–600 MB (khoảng 150–250 MB với preset JVM nhẹ) |
| PostgreSQL instance riêng | 100–250 MB |
| Database logic trên server dùng chung | khoảng 5–20 MB thêm cho mỗi database |
| Redis | 10–50 MB |

**Số app chạy được trên máy này**

| Cấu hình | Số service |
|---|---|
| **Không tối ưu** (mỗi app một DB instance, không overcommit, không ngủ) | khoảng 30–50 app backend nhỏ kèm database |
| **Bật các tính năng mục 4.9** (DB dùng chung, tinh chỉnh runtime, overcommit 1,5×, zram) | khoảng 80–120 app backend |
| **Thêm chế độ ngủ** (giả sử 50% app nhàn rỗi, ví dụ preview và side project) | khoảng 150–200 app backend |
| Site tĩnh | Hàng trăm đến hàng nghìn ở mọi cấu hình (chỉ giới hạn bởi ổ đĩa) |

Ví dụ hỗn hợp **không tối ưu**: 20 site tĩnh, 10 Node, 5 Python, 5 Go, 3 Next.js, 8 Postgres, 2 MySQL, 5 Redis = **58 service, khoảng 4,3 GB RAM**. Riêng việc chuyển 8 Postgres + 2 MySQL sang database server dùng chung đã giảm từ khoảng 1,9 GB xuống khoảng 0,6 GB.

**Ổ đĩa 200 GB** (cần GC image và giới hạn build cache):

| Nội dung | Dung lượng |
|---|---|
| Hệ điều hành | khoảng 10 GB |
| Image (giữ 3 bản để rollback, nằm cả trong Zot và containerd) | khoảng 40–60 GB |
| Build cache (giới hạn) | khoảng 25 GB |
| Log và metrics (giữ 7–14 ngày) | khoảng 10 GB |
| **Còn lại cho dữ liệu DB và volume** | **khoảng 80–100 GB** |

Backup DB nên đẩy ra S3 bên ngoài máy (R2, B2…), để máy hỏng thì backup không mất theo.

**CPU 10 core**: dư cho app ít traffic. Một app Node hay Go đơn giản xử lý được hàng nghìn request/giây trên mỗi core. CPU chủ yếu dùng cho build, nên giới hạn mỗi build tối đa 4 core.

### 9.2. Cấu hình tham khảo theo quy mô

| Quy mô | Cấu hình |
|---|---|
| Dev / cá nhân | 1 VPS 2 vCPU / 4 GB RAM, giữ tất cả vai trò (khoảng 10–20 app nhỏ khi bật DB dùng chung và chế độ ngủ) |
| Nhỏ (khoảng 100–150 app) | 1 node control+edge (2 vCPU / 4 GB), 1 build node (4 vCPU / 8 GB), 2 worker (4 vCPU / 8–16 GB) |
| Vừa (HA) | 3 control, 2 edge sau DNS/LB, 2 build, N worker. Postgres riêng có replica. |

Tách build sang máy riêng giúp mỗi worker có thêm khoảng 2,5 GB RAM cho app.

---

## 10. Rủi ro & cách giảm thiểu

| Rủi ro | Giảm thiểu |
|---|---|
| Tự viết scheduler sẽ phức tạp dần | Giữ phạm vi nhỏ (không cố làm lại k8s). Interface `Runtime` và `Scheduler` cho phép cắm k3s/Nomad backend nếu cần. |
| Database stateful khó di chuyển giữa node | Ghim DB vào node chứa volume, backup thường xuyên lên S3, có quy trình restore sang node khác. HA ở giai đoạn 4. |
| Giới hạn rate của Let's Encrypt | Dùng wildcard cert qua DNS-01 cho subdomain hệ thống, chỉ xin cert riêng cho custom domain. Dự phòng ZeroSSL. |
| Không có Docker nên debug và làm phần mạng/log khó hơn (MVP mất thêm khoảng 1–2 tuần) | Dùng `nerdctl` để debug. Dùng lại CNI plugins chuẩn thay vì tự viết. Viết test tích hợp cho agent ngay từ đầu. |
| Overcommit làm app bị OOM hoặc chậm khi nhiều app cùng tăng tải | Database luôn ở lớp `guaranteed` và không dùng swap. Agent theo dõi PSI và chủ động đẩy app ra theo lớp QoS. Tỷ lệ overcommit mặc định thấp (1,5×), chỉnh được theo node. |
| Database dùng chung: một app chiếm hết kết nối hoặc tài nguyên của app khác | Đặt `CONNECTION LIMIT` cho từng database, dùng PgBouncer, đặt `statement_timeout`, cảnh báo khi vượt quota dung lượng. Có nút nâng cấp lên dedicated. |
| Build ăn hết tài nguyên của app | Tách build node, đặt giới hạn cgroup cho BuildKit, dùng hàng đợi có giới hạn đồng thời. |
| Bảo mật khi multi-tenant | Bắt đầu với mô hình "single organization". Chỉ mở public multi-tenant khi đã có `runtime_class: sandboxed` (gVisor) hoặc microVM (Firecracker) và cô lập mạng. |
| Control plane là điểm lỗi duy nhất | Workload vẫn chạy khi control plane chết: agent và proxy giữ cấu hình cuối cùng. HA ở giai đoạn 3. |

---

## 11. Bước tiếp theo đề xuất

1. Chốt tên dự án, domain và mô hình (tự host cho 1 tổ chức hay public multi-tenant).
2. Chọn framework dashboard (SvelteKit hay React).
3. Bắt đầu Giai đoạn 0: khởi tạo monorepo Go, schema Postgres, Agent API proto và môi trường dev containerd.
