# Phase 4 — Ép `image-resolve-mode=pull`

Phụ thuộc Phase 0 (kết quả) và 3. File: `/home/hwng/github_projects/paas/internal/build/build.go` (~:135, khối `if plan.UserDockerfile != ""`).

## Việc
- Phase 0 pass: với Dockerfile user thêm `args = append(args, "--opt", "image-resolve-mode=pull")`. Dockerfile sinh ra không đổi. Test đơn vị trên danh sách args buildctl.
- Phase 0 fail (fallback): trong `vetDockerfile` từ chối file chứa `paas.local` (không phân biệt hoa thường) và build arg khai báo bằng `ARG` có giá trị chứa `paas.local/`. Ghi trong comment và báo cáo: đây là speed bump, `FROM ${A}/${B}` vẫn lọt; điều kiện xoá = có cơ chế cách ly store.
- Báo cáo: nêu độ trễ/rate limit đo ở Phase 0.

## Xong khi
Trên node build thật: `FROM paas.local/<service-khác>` fail; base image thật vẫn build.

## KHÔNG làm
Không vet text làm cơ chế chính khi `pull` hoạt động; không đổi cách đặt tên `paas.local/<serviceID>:<deploymentID>`; không lookup DB để so project.
