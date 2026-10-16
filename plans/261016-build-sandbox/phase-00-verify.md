# Phase 0 — Xác minh git trên build node

1. Trên mọi node có role `build`: `git --version` phải ≥ 2.37 (`http.curloptResolve` có từ 2.37.0).
2. Thử thật: `git -c http.curloptResolve=github.com:443:<ip thật> -c http.followRedirects=false ls-remote https://github.com/git/git HEAD` → OK. Đổi `<ip>` thành `127.0.0.1` → fail, chứng tỏ IP ghim được dùng thật.
3. Xem script cài node (`scripts/`) có cài git từ distro không. Distro cũ (Debian 11 có 2.30) → thêm yêu cầu tối thiểu vào script, hoặc fail-closed ở agent kèm thông báo rõ.

Xong khi: có version của từng node. Nếu có node <2.37, quyết định nâng cấp hay chặn trước khi giao Phase 2.

## Kết quả (2026-10-16)

- Build node duy nhất: `cs-linux-20260806212642292` (worker,build), Ubuntu 24.04, **git 2.43.0** → OK.
- `curloptResolve` với IP thật → ls-remote OK; ghim `127.0.0.1` → fail TLS → IP ghim được dùng thật.
- 11 service dùng installation, tất cả repo `https://github.com/thesunbg/<x>.git` trùng account `thesunbg` → Phase 3 không làm hỏng service nào. Hậu tố `.git` phải được chấp nhận.
