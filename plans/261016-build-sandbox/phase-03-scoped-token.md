# Phase 3 — Token GitHub scoped theo repo

File: `internal/github/github.go`, `internal/github/github_test.go`, `internal/server/github.go`, `internal/server/api_templates.go` (inspectRepo), test server.

## Thay đổi

1. `github.Client.RepoToken(ctx, installationID int64, repo string) (string, error)`: POST `/app/installations/{id}/access_tokens` với body `{"repositories":[repo],"permissions":{"contents":"read"}}`. Cache key là `(id, repo)`; đổi `tokens` sang map key struct, hoặc thêm map thứ hai nếu sạch hơn. `Token(ctx, id)` giữ nguyên cho `Repos`/`Post`/webhook.
   - Nếu `do` chưa gửi được body JSON thì mở rộng nó, đừng viết client thứ hai.
2. Server, hàm thuần `githubRepo(repoURL, account string) (name string, err error)`: `url.Parse`, `scheme == "https"`, `strings.EqualFold(u.Host, "github.com")` (không nhận `www.`, không port), path đúng `/owner/name` (bỏ `.git`, `/` cuối), `strings.EqualFold(owner, account)`. Không dùng `githubRepoRe`: regex đó khớp cả `evil.com/github.com/a/b`.
3. `gitToken(ctx, svc)` ở nhánh installation: lấy `store.GitHubInstallation(id)` → `githubRepo(svc.Source.Repo, inst.AccountLogin)` → `c.RepoToken(id, name)`. Lỗi parse thì trả lỗi rõ ("repository must be github.com/<installation account>/…"), **không** clone tiếp mà không có token.
4. `inspectRepo`: hiện đang dựng `store.Service{Source{InstallationID}}` không có Repo → thêm `Repo: req.Repo`.
5. Preview dùng `parent.Source.Repo` (cùng repo) → không cần đổi.

## Test

- `githubRepo`: `https://gitlab.com/o/r` lỗi; `https://github.com/other/r` (account=me) lỗi; `https://evil.com/github.com/me/r` lỗi; `http://github.com/me/r` lỗi; `https://github.com/Me/r.git` OK → `r`.
- Server: service có installation_id + repo gitlab → `gitToken` lỗi và fake GitHub server **không** nhận request mint (đếm số lần gọi).
- github_test: `RepoToken` gửi body có `repositories:["app"]` và `permissions.contents=="read"`; hai repo khác nhau → hai lần mint; cùng repo → cache.

## KHÔNG làm

- Gọi `Repos()` để kiểm.
- Đụng tới nhánh `GitTokenEnc`.
- Đổi `githubRepoRe` (dùng cho comment/status): sửa riêng nếu cần, không phải ở đây.
