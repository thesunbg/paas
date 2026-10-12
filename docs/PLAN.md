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
- **Scale-to-zero (tuỳ chọn)** cho app ít truy cập: proxy giữ request lại trong lúc app khởi động.
- **Tự động thêm và bớt node** qua API của nhà cung cấp cloud (Hetzner, DigitalOcean, Vultr, AWS) khi cluster thiếu hoặc thừa tài nguyên.
- **HA control plane**: 3 node control, gồm NATS cluster 3 node, Postgres primary + replica và nhiều bản control plane đặt sau load balancer.
- Đa vùng (multi-region) ở giai đoạn sau: edge node ở nhiều nơi, dùng GeoDNS.

### 4.8. Bảo mật
- Container chạy bằng crun, non-root với user namespace, seccomp/AppArmor mặc định, filesystem read-only khi có thể.
- **Cô lập mạng giữa các project** bằng nftables do agent quản lý: service khác project không thấy nhau.
- `runtime_class: sandboxed` (**gVisor**) cho code không tin cậy trên VPS thường. `runtime_class: microvm` (**Firecracker**) khi node có KVM.
- Giới hạn tốc độ request (rate limit) ở edge, bảo vệ dashboard bằng 2FA, quét image (Trivy) khi build.
- Secret không bao giờ xuất hiện trong log, bị che khi hiển thị trên UI.

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
databases ─< backups
git_installations ─< repositories
audit_logs, notifications, webhooks_in
```

- `services.spec` (JSONB) lưu **trạng thái mong muốn**: image, số replica, tài nguyên, port, health check.
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
│   ├── scheduler/        # placement, bin-packing
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
- PostgreSQL, MySQL và Redis 1-click, kèm backup lên S3.
- Dashboard: project, service, deploy, log, env, domain. CLI cơ bản.
- **Kết quả**: thay thế được Coolify cho 1 máy chủ.

### Giai đoạn 2: Cluster (6–8 tuần)
- Lệnh `join` với token, WireGuard mesh, CA nội bộ, mTLS.
- Scheduler (bin-packing, affinity, volume pinning) và reconciler tự phục hồi.
- Nhiều replica, load balancing giữa các node, DNS nội bộ.
- Tách vai trò edge, build và worker. Drain/cordon node.
- Metrics (VictoriaMetrics) và logs (VictoriaLogs), biểu đồ trên dashboard, cảnh báo.
- Preview deployment cho PR, cập nhật GitHub Check Run.

### Giai đoạn 3: Mở rộng & tối ưu (6–8 tuần)
- Horizontal autoscaling (CPU, RAM, RPS) và scale-to-zero.
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
- Đa vùng, GeoDNS, CDN cho site tĩnh.
- Terraform provider, file cấu hình `paas.toml`, API công khai.

---

## 9. Yêu cầu phần cứng tham khảo

| Quy mô | Cấu hình |
|---|---|
| Dev / cá nhân | 1 VPS 2 vCPU / 4 GB RAM, giữ tất cả vai trò |
| Nhỏ (≈ 50 app) | 1 node control+edge (2 vCPU / 4 GB), 1 build node (4 vCPU / 8 GB), 2 worker (4 vCPU / 8 GB) |
| Vừa (HA) | 3 control, 2 edge sau DNS/LB, 2 build, N worker. Postgres riêng có replica. |

---

## 10. Rủi ro & cách giảm thiểu

| Rủi ro | Giảm thiểu |
|---|---|
| Tự viết scheduler sẽ phức tạp dần | Giữ phạm vi nhỏ (không cố làm lại k8s). Interface `Runtime` và `Scheduler` cho phép cắm k3s/Nomad backend nếu cần. |
| Database stateful khó di chuyển giữa node | Ghim DB vào node chứa volume, backup thường xuyên lên S3, có quy trình restore sang node khác. HA ở giai đoạn 4. |
| Giới hạn rate của Let's Encrypt | Dùng wildcard cert qua DNS-01 cho subdomain hệ thống, chỉ xin cert riêng cho custom domain. Dự phòng ZeroSSL. |
| Không có Docker nên debug và làm phần mạng/log khó hơn (MVP mất thêm khoảng 1–2 tuần) | Dùng `nerdctl` để debug. Dùng lại CNI plugins chuẩn thay vì tự viết. Viết test tích hợp cho agent ngay từ đầu. |
| Build ăn hết tài nguyên của app | Tách build node, đặt giới hạn cgroup cho BuildKit, dùng hàng đợi có giới hạn đồng thời. |
| Bảo mật khi multi-tenant | Bắt đầu với mô hình "single organization". Chỉ mở public multi-tenant khi đã có `runtime_class: sandboxed` (gVisor) hoặc microVM (Firecracker) và cô lập mạng. |
| Control plane là điểm lỗi duy nhất | Workload vẫn chạy khi control plane chết: agent và proxy giữ cấu hình cuối cùng. HA ở giai đoạn 3. |

---

## 11. Bước tiếp theo đề xuất

1. Chốt tên dự án, domain và mô hình (tự host cho 1 tổ chức hay public multi-tenant).
2. Chọn framework dashboard (SvelteKit hay React).
3. Bắt đầu Giai đoạn 0: khởi tạo monorepo Go, schema Postgres, Agent API proto và môi trường dev containerd.
