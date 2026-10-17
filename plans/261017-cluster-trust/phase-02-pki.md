# Phase 2: CA nội bộ

## Việc
1. Tạo package mới `internal/pki` (nếu có package sẵn làm được việc này thì dùng nó; kiểm tra `grep -rn x509 internal` trước khi tạo):
   - `NewCA() (certPEM, keyPEM []byte, err)`: ECDSA P-256, `IsCA`, hạn 10 năm, CN `paas cluster CA`.
   - `Issue(caCert, caKey, names []string, ttl, client bool) (tls.Certificate, error)`: ExtKeyUsage là server auth, cộng thêm client auth khi `client` là true.
   - `ServerTLS(ca) *tls.Config`: dùng `GetCertificate` để trả cert đã cache và tự mint lại khi còn dưới 30 ngày. Cert có SAN `nats.paas.internal`, hạn 1 năm, `MinVersion` TLS 1.2.
   - `RouteTLS(ca) *tls.Config`: cert vừa server vừa client, `ClientAuth: RequireAndVerifyClientCert`, `ClientCAs`/`RootCAs` là CA, `ServerName` là `nats.paas.internal`.
2. `internal/server/server.go`: thêm setting `cluster_ca_cert` (PEM) và `cluster_ca_key` (đã mã hoá bằng `secrets.Box`, giống cách mã hoá env). Load hoặc tạo theo đúng mẫu `cluster_token` ở `server.go:134`. Khi chạy HA, hai control plane khởi động cùng lúc có thể cùng tạo CA, nên dùng insert-if-absent rồi đọc lại. Xem `SetSetting` đã làm được upsert-do-nothing chưa; chưa thì thêm `SetSettingIfAbsent`.
3. Cung cấp `s.caPEM` cho phase 3, để trả về trong response của enroll.

## Tiêu chí xong
- Unit test: một client với `RootCAs` là CA và ServerName `nats.paas.internal` verify được cert do `ServerTLS` cấp. Một CA khác thì fail.
- Test: chỉnh đồng hồ giả hoặc cert gần hết hạn, `GetCertificate` mint ra cert mới.
- `go test ./internal/pki/ ./internal/server/` xanh.

## KHÔNG làm
- Rotate CA.
- Cert phía client cho node. Node xác thực bằng NKey, không dùng mTLS.
- Dùng cert ACME.
