# Brainstorm — Hộp cát cho build (việc B)

Ngày: 2026-10-16

## Vấn đề

Kiểm tra path/URL trên build node chỉ so chuỗi. Không resolve đường dẫn thật, không kiểm tra host đích. Hậu quả:

1. **Symlink thoát repo.** Repo có `x -> /` và `root_dir: x` → `root = /` → `buildctl --local context=/` gửi cả FS của host vào build. `strings.HasPrefix(root, src)` vẫn qua vì không resolve.
2. **Đọc/ghi file host qua symlink** (rộng hơn phạm vi ban đầu, phát hiện khi scout):
   - `exampleEnv` (build.go:465) có `Lstat` file, nhưng không chặn được thư mục cha là symlink. Inspect trả nội dung về thẳng user (vd `/etc/paas/agent.env`).
   - `detect.Detect` đọc `package.json`, `go.mod`… và `declaredArgs`/`exposedPort` đọc Dockerfile. `opt.Dockerfile` không được validate (`../../x`).
   - `ensureDockerignore` (build.go:309): `Stat` symlink treo thì lỗi → `WriteFile` đi theo link → **ghi được file tuỳ ý trên host** (`.dockerignore -> /etc/cron.d/x`).
3. **Cache mount dùng chung giữa tenant.** `type=cache` không có `id` nên mặc định id = target, mọi build trên daemon dùng chung. Tenant A đầu độc được `/go/pkg/mod` hay `/root/.cache/pip` của B.
4. **Token installation đi theo URL tuỳ ý.** `gitToken` mint token full-scope cho installation, không xét `svc.Source.Repo`. Service có `installation_id` cộng repo `https://evil.com/x` thì token rơi vào tay evil.com, và token đó đọc/ghi được mọi repo của installation.
5. **Clone SSRF.** Cho phép `http://`, không kiểm tra IP đích; git tự theo redirect.

## Các phương án đã cân nhắc

| Vấn đề | Chọn | Loại | Lý do loại |
|---|---|---|---|
| Symlink | **Lọc một lần sau clone**: xoá symlink nào có EvalSymlinks lỗi hoặc trỏ ra ngoài src | Kiểm tra từng điểm (EvalSymlinks+Rel ở 85/358 + Dockerfile) | Còn sót exampleEnv qua dir cha, detect, `.dockerignore`. N chỗ check, chỗ thứ N+1 sẽ quên |
| | | `core.symlinks=false` | Làm hỏng symlink nội bộ (monorepo, `.env -> .env.prod`) |
| Token | **Mint scoped**: `repositories:[name]`, `permissions:{contents:read}`, server kiểm host==github.com, owner==AccountLogin | Kiểm qua `Repos()` | Tốn thêm tới 10 lần gọi API, token vẫn full-scope |
| Clone | **Resolve trong Go + ghim IP** qua `http.curloptResolve`, tắt redirect, chỉ cho https | Chỉ resolve rồi kiểm | Còn TOCTOU (DNS rebinding) và redirect |
| Cache id | **Theo project** `id=<projectID><target>` | Team | Các project trong cùng team đầu độc được nhau |
| | | Service | Ít cache hit, mà không thêm được ranh giới nào có ý nghĩa |

## Giải pháp chốt

1. `sanitizeSymlinks(src)` gọi trong `checkout()` sau khi xoá `.git`. Từ lúc đó mọi đọc/ghi dưới `src` đều an toàn, và `filepath.Clean("/"+x)` là đủ cho RootDir/Dockerfile/ComposePath. Không thêm check theo từng điểm.
2. Hàm kiểm IP public (`netguard.Public(ip)` hoặc tương đương) tách ra từ `notify.go:37-38`. Hai người dùng thật là notify và clone.
3. `checkout()`: URL `https://` (hoặc file khi `AllowLocalGit`). Resolve host, bất kỳ IP nào không public → từ chối. Git chạy với `-c http.curloptResolve=<host>:<port>:<ip> -c http.followRedirects=false -c protocol.allow=never -c protocol.https.allow=always` (và `protocol.file.allow=always` khi local).
4. `github.Client.RepoToken(ctx, id, repo)` mint token scoped, cache theo `(id, repo)`. `Token()` giữ nguyên cho `Repos`/`Post`. Server `gitToken` + `inspectRepo`: parse URL bằng `url.Parse`, host phải đúng `github.com`, owner phải trùng `AccountLogin` của installation (không thì báo lỗi). Mint với tên repo.
5. `proto.BuildRequest.ProjectID` → `detect.Options.CacheScope`. Mọi `type=cache` sinh ra đều đi qua một helper và được gắn `id=<scope><target>`.

## Rủi ro / lưu ý

- **git trên build node phải ≥2.37** (`http.curloptResolve`). Máy dev là 2.56; node thật thì phải kiểm (Phase 0). Nếu thấp hơn: fail-closed, báo lỗi rõ, không lặng lẽ hạ xuống resolve-only.
- **Cache id chỉ là defense-in-depth.** Dockerfile do user viết vẫn tự chọn `id`, kể cả id của project khác, nếu biết UUID. Project ID là UUID ngẫu nhiên và không lộ ra ngoài team → đủ cho giai đoạn này. Cách ly thật (BuildKit theo tenant hoặc cache namespace) để sau.
- **Rollout lệch phiên bản:** paasd cũ + agent mới thì `ProjectID` rỗng → không gắn id (hành vi cũ). Chấp nhận, ghi trong comment kèm điều kiện xoá: khi mọi paasd đã gửi ProjectID.
- **Token cá nhân (`GitTokenEnc`)** vẫn được gắn vào mọi URL https. Token của chính user, user tự đặt URL → không phải lỗ hổng xuyên tenant. Không làm.
- Service đang có installation_id cộng repo không phải GitHub sẽ bắt đầu build fail. Đó là đúng ý. Ghi vào release note.
- Repo `github.com/Owner/Repo` khác hoa thường so với AccountLogin → so sánh không phân biệt hoa thường.

## Tiêu chí xong

- Test: repo có `x -> /`, `root_dir=x` → lỗi "not found". Link `.dockerignore` treo bị xoá, không tạo file bên ngoài. Symlink nội bộ `a -> b` vẫn còn.
- Test: service có installation_id + repo `https://gitlab.com/o/r` → `gitToken` lỗi, không gọi mint. Repo github.com nhưng owner khác → lỗi. Body mint có `repositories:["r"]`.
- Test: golden/assert mọi dòng `type=cache` trong Dockerfile sinh ra đều có `id=<scope>`.
- Test: clone `http://…`, `https://127.0.0.1/…`, `https://localhost/…` → từ chối trước khi chạy git.
- `go test ./internal/build/... ./internal/detect/... ./internal/github/... ./internal/server/...` xanh.

## Bước tiếp

Plan: `plans/261016-build-sandbox/plan.md`.
