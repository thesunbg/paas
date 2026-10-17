# Phase 2 — runDeployment chỉ lock khi rollout

File: `internal/server/deploy.go`. Chỉ sửa `runDeployment`.

## Vì sao an toàn

`ClaimQueuedDeployment` đã loại các service đang có deployment ở trạng thái `building` hoặc `deploying`, nên DB đã đảm bảo mỗi service chỉ có một deploy tại một thời điểm. Pha build chỉ đọc service và ghi vào bảng deployments. Pha này không đụng vào instance, nên không cần lock.

## Thay đổi

1. Bỏ `unlock := s.lock(d.ServiceID); defer unlock()` ở đầu hàm.
2. Phần từ đầu hàm tới hết `switch` (status `deploying`, commit status pending, build/image) chạy không lock, giữ nguyên như hiện tại.
3. Ngay sau `switch`, thay `svc, _ = s.store.ServiceByID(ctx, d.ServiceID)` bằng:

   ```go
   // Build ran unlocked; the service may have been stopped or deleted meanwhile.
   unlock := s.lock(d.ServiceID)
   defer unlock()
   svc, err = s.store.ServiceByID(ctx, d.ServiceID)
   if err != nil {
       _ = s.store.SetDeploymentStatus(ctx, d.ID, model.DeployCancelled, "service deleted during build")
       return
   }
   if svc.Status == model.StatusStopped {
       s.hub.Log(d.ID, "==> service was stopped during build; not rolling out")
       _ = s.store.SetDeploymentStatus(ctx, d.ID, model.DeployCancelled, "service stopped during build")
       s.reportCommitStatus(ctx, svc, d, "error")
       return
   }
   ```

   Phần còn lại của hàm (cron, static, rollout, active, supersede, notify) giữ nguyên và chạy dưới lock.
4. Các nhánh `failDeployment` trong pha build vẫn chạy không lock, giống hiện tại về mặt dữ liệu.

## Xác minh

- `go build ./... && go vet ./internal/server/`.
- Đọc lại toàn bộ hàm: từ sau dòng `defer unlock()` không còn chỗ nào dereference `svc` mà chưa kiểm tra `err`.

## KHÔNG làm

- Không tách `runDeployment` thành `buildPhase` và `rolloutPhase`. Hàm vẫn đọc tuần tự được; chỉ cần dời một chỗ lấy lock.
- Không đổi `ClaimQueuedDeployment` hay `failDeployment`.
- Không xử lý trường hợp service bị cho ngủ trong lúc build. Rollout sẽ bật service lên lại, chấp nhận được.
- Không thêm trạng thái mới cho service hay deployment. `DeployCancelled` đã có sẵn.
