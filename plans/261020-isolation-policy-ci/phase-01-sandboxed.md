# Phase 1 — sandboxed fail-closed + tài liệu

File: `/home/hwng/github_projects/paas/internal/runtime/runtime.go` (:364 `runtimeFor`, caller :414), `README.md` dòng 6, `docs/PLAN.md`.

## Việc
- `runtimeFor(class) (string, any, error)`: nhánh `RuntimeSandboxed` không tìm thấy `containerd-shim-runsc-v1` → `return "", nil, fmt.Errorf("sandboxed runtime requested but gVisor (containerd-shim-runsc-v1) is not installed on this node")`. Bỏ `slog.Warn`.
- `createContainer` trả lỗi đó, không tạo container.
- Test: bảng đơn vị cho `runtimeFor` (không gVisor + sandboxed → lỗi; standard → runc như cũ). Dùng PATH rỗng/tạm.
- Tài liệu: README + PLAN.md ghi `standard` = namespaces + seccomp + drop cap, root trong container, không userns; kernel/runc CVE thoát được host; tenant không tin cậy phải dùng `sandboxed`.

## Xong khi
`go test ./internal/runtime/...` xanh; tài liệu có câu nói rõ ranh giới.

## KHÔNG làm
Không thêm userns; không đổi scheduler (đã lọc capability); không thêm cờ cấu hình "cho phép fallback".
