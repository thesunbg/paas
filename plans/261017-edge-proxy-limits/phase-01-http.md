# Phase 1: giới hạn của server HTTP

## Việc

1. `internal/proxy/proxy.go`: thêm helper
   ```go
   // edgeServer builds a public-facing server. No Read/WriteTimeout: they would cut
   // SSE, websockets and long uploads/downloads; ReadHeaderTimeout covers slowloris.
   func edgeServer(addr string, h http.Handler) *http.Server {
       return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 15 * time.Second,
           IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
   }
   ```
   Thay 3 chỗ dựng server ở các dòng 150, 151 và 164 bằng `edgeServer(...)`. `httpsSrv` vẫn gán `TLSConfig = tlsCfg` sau khi tạo.
2. `internal/server/server.go:195`: thêm trực tiếp `IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10` vào literal. Không dùng helper ở đây vì khác package và chỉ có một người dùng.
3. Test trong `internal/proxy/proxy_test.go`: dựng `edgeServer` trên `127.0.0.1:0` với handler trả 200.
   - Header `X-Big` 100 KB phải nhận 431.
   - Header 10 KB phải nhận 200.

## Tiêu chí xong
- `go test -race ./internal/proxy/ -run EdgeServer` xanh.
- `grep -n "http.Server{" internal/proxy/proxy.go` chỉ còn đúng 1 chỗ (trong helper).

## KHÔNG làm
- `ReadTimeout` / `WriteTimeout`.
- Sửa `internal/agent/agent.go:505`.
- Biến các giá trị trên thành config.
- Test cho `IdleTimeout` (đó là hành vi của stdlib).
