# Phase 5 — deploy.yml + user deploy

File: `/home/hwng/github_projects/paas/.github/workflows/deploy.yml`, wrapper mới `scripts/paas-deploy` (cài vào `/usr/local/sbin/paas-deploy`), `internal/installer/install.sh` chỉ đọc (không sửa trừ khi cần).

**Làm trước các phase khác nếu mật khẩu root chưa đổi.**

## Server (thứ tự cố định)
1. Tạo user `deploy`, `~deploy/.ssh/authorized_keys` (key mới), thư mục `~deploy/stage/` (0700).
2. Cài `/usr/local/sbin/paas-deploy` (root:root 0755): đọc paasd/paas/install.sh chỉ từ `~deploy/stage/`, copy sang thư mục root-owned rồi chạy `install.sh server --binary <bản copy>`. Tham số `--domain`, `--email`, `--dashboard-domain`, `--https` validate regex chặt; không nhận `--binary`, URL hay đường dẫn tuỳ ý.
3. `/etc/sudoers.d/paas-deploy`: `deploy ALL=(root) NOPASSWD: /usr/local/sbin/paas-deploy`, kiểm bằng `visudo -c`.
4. Chạy thử deploy bằng user mới (Phase workflow bên dưới).
5. Rồi mới: `PasswordAuthentication no`, `PermitRootLogin no`, reload sshd (giữ phiên root cũ mở). Đổi mật khẩu root. Xoá secret `DEPLOY_PASSWORD`.

## Workflow
- Bỏ inputs host/port/user, bỏ default IP; `PAAS_DEPLOY_HOST/PORT/USER` từ `vars`. `concurrency.group` dùng hằng.
- Bỏ `sshpass`, `apt install`, nhánh mật khẩu. `DEPLOY_SSH_KEY` bắt buộc, `DEPLOY_KNOWN_HOSTS` vào `~/.ssh/known_hosts`, `StrictHostKeyChecking=yes`.
- `environment: production`; bảo vệ nhánh `deploy` (cài đặt repo, ghi vào báo cáo).
- Upload vào `~deploy/stage/`, chạy `sudo /usr/local/sbin/paas-deploy <args>`, bước Verify dùng user deploy (đọc `journalctl` cần nhóm `systemd-journal`/`adm`, hoặc để wrapper in log).

## Xong khi
Deploy chạy bằng `deploy`; `ssh root@...` và đăng nhập mật khẩu bị từ chối; test wrapper với đầu vào xấu (`--binary /x`, `--domain 'a;b'`, tham số thừa) đều bị từ chối.

## KHÔNG làm
Không ký/kiểm chữ ký binary (key deploy vẫn ≈ root; ghi rõ trong báo cáo); không `from=` trong authorized_keys; không xoá IP khỏi lịch sử git.
