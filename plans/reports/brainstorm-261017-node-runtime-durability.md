# Brainstorm: F — độ bền của runtime trên node

## Vấn đề

- **Reboot host thì container không lên lại.** netns là bind-mount trong `dataDir/netns`. Sau reboot, file vẫn còn nhưng không còn là nsfs. Đường dẫn này lại nằm cứng trong OCI spec (`runtime.go:340`), nên `NewTask` → crun `setns` lỗi. `Recover` chỉ log lỗi rồi bỏ qua.
- **Log tăng không giới hạn trong lúc chạy.** `rotateLog` chỉ chạy lúc `startTask`, bằng rename. Container chạy lâu thì `output.log` cứ thế phình ra.
- **Follow phình RAM.** Cả `reader.ReadString('\n')` lẫn `partial += line` (`metrics.go:133-142`) đều không có giới hạn, nên 1 GB không có `\n` nằm hết trong RAM của agent.
- **Leak trong Run.** `writeNetFiles` lỗi sau khi `Attach` thành công (`runtime.go:249`) thì return luôn, IP trong IPAM và netns không được giải phóng. `createContainer` lỗi thì có dọn net nhưng để lại `containerDir`.
- **Nhánh orphan** (`runtime.go:726-729`): `ctr.Delete` thất bại nếu task còn sống, và netns không được dọn.
- **Recover chạy lại job Once.** Sau reboot, migration hoặc backup chạy lại mà không ai chờ kết quả. Nếu task còn sống (agent restart chứ không phải reboot) thì không ai `Remove` nó, nên container treo vĩnh viễn.

## Sự thật đã xác minh

- Shim runc v2 mở file log bằng `O_APPEND|O_CREATE|O_WRONLY` (containerd v2.4.1 `cmd/containerd-shim-runc-v2/process/io.go:116`). Vì vậy truncate(0) khi shim vẫn đang ghi là an toàn: lần ghi kế tiếp bắt đầu từ offset 0, không sinh sparse hole.
- `Follow` đã có nhánh "size < pos → seek 0". Với copytruncate, inode giữ nguyên nên Follow tự đi tiếp. Với rename như hiện tại, Follow vẫn giữ fd cũ và đọc một file chết.
- `netns.LoadNetNS(p).Closed()` trả `true` khi path không tồn tại hoặc không phải nsfs. Trong trường hợp sau, hàm tự xoá file rác. Không cần tự viết statfs.
- `netns.NewNetNS` luôn sinh tên ngẫu nhiên, không chọn được path. Vì vậy sau khi tạo lại netns phải cập nhật spec của container (`ctr.Update` + `WithSpec`).
- IPAM host-local lưu ở `dataDir/cni/...` (`network.go:140`), tức là trên đĩa, nên reservation cũ vẫn còn sau reboot. Phải `Detach` trước khi `Attach`, như `Run` đang làm ở dòng 242.
- **IP có thể đổi sau reboot, và như vậy vẫn ổn.** Heartbeat 5s gửi `ContainerStatus.IP`, rồi `ApplyContainerStatus` ghi `ip=CASE WHEN $6<>'' ...` (`store/nodes.go:233`). Proxy upstream (`routes.go:44`, `reconcile.go:367`) và mesh (`mesh.go:76`) đều đọc IP từ DB. Không có chỗ nào cache IP ngoài DB.
- Agent gọi `SyncFirewall` trước `Recover` (`agent.go:117`). DNS listen trên gateway có thể lỗi lần đầu vì bridge chưa có, nhưng `EnsureDNS` chạy mỗi 5s sẽ bắt lại.
- `tailFile` đã giới hạn đọc 1 MB, nên không phải nguồn phình RAM.

## Quyết định (đã chốt với user)

| Điểm | Chọn | Loại |
|---|---|---|
| Job Once khi Recover | Kill + Remove tất cả | Cho chạy xong rồi Remove: có thể chạy song song với lần retry, và kết quả không ai đọc |
| Xoay log | copytruncate, 10 MB, 1 bản `.1`, kiểm tra trong tick 5s của agent và lúc `startTask` | logger `binary://`: thêm một process cho mỗi container và thêm một binary |
| Dòng dài trong Follow | Cắt ở 64 KB + `…[truncated]`, bỏ phần còn lại tới `\n` | Chia thành nhiều chunk: 1 GB thành khoảng 16 nghìn message NATS tới trình duyệt |
| Orphan | Kill task + xoá container + dọn netns lấy từ spec | Quét thư mục không có container: thêm một vòng quét để đổi lấy một cửa sổ crash cỡ micro giây |

### Sửa lại quyết định orphan (b), có chủ ý

Phương án đã chốt gồm cả "(b) Remove chỉ RemoveAll khi `ctr.Delete` thành công". **Đã bỏ (b).** Nếu giữ `state.json` khi `ctr.Delete` lỗi, `Recover` sẽ nạp lại container đó như một container đang được quản lý (`!Stopped`), rồi **khởi động lại chính container mà control plane vừa ra lệnh xoá**. Thứ tự hiện tại của `Remove` là Stop → xoá khỏi map → `ctr.Delete` → Detach → xoá netns → RemoveAll. Nếu `ctr.Delete` lỗi thì IP vẫn đã được trả, và lần Recover sau đi vào nhánh orphan (a) để dọn nốt. Như vậy chỉ (a) là đủ.

## Thiết kế

### 1. netns: sửa tại một chỗ duy nhất trong `startTask`

`startTask` là điểm chung của `Recover`, `watch` (crash restart) và `Unfence`. Container đã fence hoặc đang ngủ trước khi reboot sẽ được start về sau qua chính hàm này, nên sửa ở `Recover` thôi là thiếu.

Tách helper `attachNet(ctx, id, m) (*netns.NetNS, error)` từ phần `Run:236-248`: tạo netns, `Detach` (dọn reservation cũ), `Attach`, gán `m.NetNS`/`m.IP`. Nếu `Attach` lỗi thì xoá netns. Helper này có **hai** người dùng thật: `Run` và `ensureNetNS`.

`ensureNetNS(ctx, ctr, id)` được gọi trong `startTask` trước `NewTask`:
- Lấy `m`. Nếu `nil`, hoặc `Closed()` trả `false`, thì return luôn. Hot path chỉ tốn một lần open + close fd.
- Gọi `attachNet`, sau đó `ctr.Spec`, thay `Path` của namespace network bằng path mới, rồi `ctr.Update(ctx, client.UpdateContainerOpts(client.WithSpec(spec)))`, rồi `saveState`.
- IP mới tới DB qua heartbeat kế tiếp, do `OnChange` gọi `heartbeat` ngay sau start.

### 2. Run: dọn sạch khi lỗi

Sau khi `attachNet` thành công, mọi lỗi trước khi container được đưa vào map đều phải `Detach`, `nsh.Remove()` và `RemoveAll(containerDir)`. Dùng một `defer` có cờ `ok`, để không phải chép lặp lại ở ba nhánh lỗi.

### 3. Recover

- Container không có `state.json`: `task.Delete(WithProcessKill)` nếu có task, rồi `ctr.Delete(WithSnapshotCleanup)`. netns lấy path từ `ctr.Spec` (namespace network) rồi `LoadNetNS(p).Remove()`. IP thì `Remove` gốc đã Detach trước khi container trở thành orphan.
- `m.Req.Once`: đăng ký vào map (để `Remove` tìm được `m` mà Detach), rồi `r.Remove(ctx, id, time.Second)` và `continue`. Không bao giờ `startTask`.

### 4. Log

- Thay `rotateLog` (rename) bằng `capLog(path)`. Nếu `size >= 10 MB`: copy **10 MB cuối** sang `path+".1.tmp"`, rename thành `.1`, rồi `os.Truncate(path, 0)`. Copy phần đuôi thay vì cả file, vì file có thể đã lên tới vài trăm MB giữa hai tick; như vậy công việc mỗi tick bị chặn ở 10 MB. `io.Copy` dùng buffer 32 KB, nên RAM cũng bị chặn.
- `Runtime.CapLogs()` duyệt id trong map và gọi `capLog`. Agent gọi hàm này trong `tick` 5s sẵn có. Không thêm ticker mới.
- `startTask` gọi `capLog` thay cho `rotateLog`.
- Byte ghi trong khoảng giữa copy và truncate sẽ mất. Chấp nhận được với log.

### 5. Follow

- `bufio.NewReaderSize(f, 64<<10)` + `ReadSlice('\n')`.
- `ErrBufferFull`: emit `partial + chunk` (cắt tối đa 64 KB) + `…[truncated]`, rồi đặt `skip=true`. Ở chế độ skip, bỏ dữ liệu cho tới `\n` kế tiếp.
- EOF chưa gặp `\n`: copy chunk vào `partial`. Nếu `len(partial)` vượt 64 KB thì xử lý như trường hợp truncated. Phải copy vì slice của `ReadSlice` mất hiệu lực sau lần đọc kế tiếp.
- Nhánh seek-0 khi file bị truncate: reset `partial` và `skip`.

## Phương án đã loại

- **Không giữ netns file, CNI attach vào `/proc/<pid>/ns/net` sau `NewTask`.** Theo cách này reboot sẽ tự lành, nhưng IP đổi ở **mỗi** lần crash restart (hiện nay IP ổn định), và CNI DEL phải chạy trước khi netns chết. Thay đổi lớn hơn và kém ổn định hơn. Loại.
- **Giữ IP cũ sau reboot bằng CNI arg `IP=`.** Heartbeat đã lan truyền IP mới trong khoảng 5s, và sau reboot thì mọi container trên node đằng nào cũng có khoảng gián đoạn. YAGNI.
- **Agent tự giữ pipe stdout (`cio.WithStreams`) để tự xoay log.** Agent restart thì mất pipe, phải làm fifo reattach. Loại.
- **Ghi `state.json` trước `createContainer`.** Chỉ đổi cửa sổ crash từ phía này sang phía kia. Loại.

## Rủi ro và nợ ghi nhận

- Agent crash trong khoảng giữa `Attach` và `saveState` của `Run`, cửa sổ cỡ micro giây, sẽ để lại một IP và một netns bị leak. Không quét thư mục để bắt trường hợp này (đã chốt).
- Agent crash giữa Stop và `ctr.Delete` trong `Remove`: state vẫn còn, nên `Recover` khởi động lại container. Đây là lỗi có từ trước, không thuộc F. Control plane phải tự dọn instance lạ.
- `tailFile` có thể trả một dòng gần 1 MB, có thể vượt max payload của NATS khi reply `Logs`. Không ảnh hưởng RAM. Không sửa ở đây.
- Burst 1 GB trong vòng 5s vẫn chiếm 1 GB đĩa cho tới tick kế tiếp. Tiêu chí xong là về RAM, nên chấp nhận được.
- Job Once đang chạy khi agent restart (không reboot) sẽ bị kill giữa chừng. Đã chốt.

## Tiêu chí xong / kiểm chứng

1. **Reboot (integration, root):** chạy một container HTTP. Giả lập reboot bằng cách kill task + `task.Delete` + `netns.Remove()` (unmount) rồi `Close` runtime. Sau đó `New` + `Recover`. Container phải running và health check phải pass trên IP mới (có thể trùng IP cũ).
2. **Unfence sau reboot:** như (1), nhưng container ở trạng thái Fenced. `Recover` không start nó. Sau đó `Unfence` thì start được.
3. **Job Once:** `Run` với `Once` (`sleep 60`), rồi `Recover` trên runtime mới. Container phải biến mất khỏi containerd, thư mục bị xoá, và không còn task nào.
4. **Follow (unit, không cần containerd):** ghi 200 MB không có `\n` vào file đang được follow, sau đó ghi `"ok\n"`. Kiểm tra: dòng đầu dài ≤ 64 KB + marker; dòng thứ hai là `"ok"`; `runtime.MemStats.HeapInuse` tăng < 8 MB.
5. **capLog (unit):** file 15 MB thành `.1` 10 MB đúng đuôi, file gốc 0 byte. Mở fd `O_APPEND` trước, ghi sau truncate, rồi kiểm tra size bằng đúng số byte vừa ghi.
6. **Run leak (unit hoặc integration):** cho `writeNetFiles` lỗi (dir chỉ đọc), rồi kiểm tra netns dir rỗng và IPAM dir không còn reservation.
7. `go vet ./...` và `go test ./...` xanh.
