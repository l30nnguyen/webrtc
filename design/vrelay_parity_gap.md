# Việc còn thiếu so với dịch vụ cũ (vrelay) — bản dành cho người không chuyên kỹ thuật

Cập nhật: 2026-10-19. Nguồn so sánh: `SERVICE_SPEC.md` (mô tả dịch vụ cũ) và hiện trạng code của dự án này.
Trạng thái: **bản nháp đã review một vòng**, chưa có việc nào bắt đầu. Dự án chưa rollout.

## 1. Tóm tắt trong một đoạn

Dự án này là phiên bản kế tiếp, làm theo cách khác dịch vụ cũ: video đi thẳng giữa camera và điện thoại (WebRTC), server chỉ làm "tổng đài" giúp hai bên tìm thấy nhau. Phần lớn tính năng của dịch vụ cũ đã có hoặc không còn cần. Còn lại **một nhóm việc nhỏ về vận hành server**, **một việc về nhiều server**, **một việc về TURN**, **một việc cho camera ở Trung Quốc**, và **một quyết định về cách hai hệ thống SID cùng tồn tại**. Nguyên tắc chọn việc: *server càng đơn giản càng tốt; việc nào app hoặc camera làm được thì để app hoặc camera làm.*

## 2. Quyết định đã có

| Chủ đề | Quyết định |
| --- | --- |
| Phía camera và app (client) | Coi như đã đủ. Chỉ còn xử lý camera ở Trung Quốc. Không cần xử lý loại NAT vì đã có TURN |
| Phía server | Giữ đơn giản để ít lỗi và dễ mở rộng. Việc nào app hoặc camera làm được thì giao cho app hoặc camera |
| Chạy thử song song | Chạy hai dịch vụ (cũ và mới) song song, độc lập, không dùng chung dữ liệu. Tác động và hiện trạng VPS ghi ở mục 5 |

## 2.1 Kiến trúc đích (bạn chốt 2026-10-19)

**Toàn bộ dịch vụ chỉ một chương trình (một file chạy) và chỉ một cổng.** Cổng đó đảm nhận phần "điều khiển" như vrelay. Phần chuyển tiếp video là máy chủ TURN, chạy riêng trên một hoặc nhiều máy khác.

| Thành phần | Chạy ở đâu | Hiện trạng so với đích |
| --- | --- | --- |
| Chương trình chính (`signaling-server`) | Một file duy nhất, một cổng | **Đã gần đúng.** Một file duy nhất đã gồm tổng đài, kích hoạt SID (Center) và tải file cho camera, tất cả trên một cổng HTTPS. Chương trình Center riêng không còn nữa. Bật phần Center bằng cờ `-center` |
| Chuyển tiếp video | Máy chủ TURN riêng, một hoặc nhiều máy | **Đã đúng hướng.** Violet chạy riêng. Còn thiếu: danh sách nhiều TURN và tài khoản tạm thời (việc 5) |
| Công cụ cấp SID (`sidctl`) | Máy của người quản trị, không nằm trên server | Giữ là chương trình riêng vì nó giữ khoá bí mật dùng để ký SID. Đặt khoá đó lên server là hạ mức an toàn. **Đây là suy luận của mình, cần bạn xác nhận** |

Những chỗ hiện chưa đúng "một cổng":

| Vấn đề | Giải thích | Cách xử lý |
| --- | --- | --- |
| Gia hạn chứng chỉ dùng thêm cổng 80 | nginx giữ cổng 80 chỉ để Let's Encrypt kiểm tra tên miền | Cho chính chương trình tự xin và gia hạn chứng chỉ ngay trên cổng 443 (cách này không cần cổng 80), hoặc xác thực qua DNS, hoặc đặt sau bộ cân bằng tải đã lo chứng chỉ. Cách nên chọn: tự gia hạn trong chương trình, vì bỏ được nginx và `renewcert.sh` |
| Cấu hình TURN nằm trong tập tin cục bộ (`cfg/turn.conf`) | Nhiều máy thì phải chép tập tin này đồng bộ | Cho Center (cùng chương trình) cấp danh sách TURN cho app và camera, hoặc đọc từ một tập tin dùng chung |
| Hai bản chạy cùng chương trình | Bản "production" (443) đang ở chế độ cũ không có Center, bản dev (8443) có Center | Khi rollout chỉ còn một bản, bật `-center` |

Tác động lên việc chạy song song với vrelay: dịch vụ mới chỉ cần **một cổng**, không cần cổng 80, không cần TURN trên cùng máy. Vì thế các lo ngại về trùng cổng 80 và tranh băng thông TURN ở mục 5 giảm đi nhiều. Dịch vụ mới chỉ cần nhường 443 (và có thể chạy ở 8443) cho vrelay.

## 3. Việc cần làm

Cột "Ai làm": **Server** là chương trình chính, **Camera** là phần mềm trên camera, **App** là ứng dụng điện thoại (đội app của bạn).
Cột "Cỡ": N = nhỏ, V = vừa, L = lớn. Đây là ước lượng thô để xếp thứ tự, chưa phải cam kết thời gian.
Thứ tự theo quyết định C: vận hành trước, rồi mở rộng, cuối cùng là việc để sau.

### Nhóm 1 — Vận hành (làm trước)

| # | Việc | Giải thích đơn giản | Ai làm | Cỡ |
|---|---|---|---|---|
| 1 | Tắt server "êm" | Khi khởi động lại hoặc nâng cấp, hiện server dừng đột ngột và mọi camera bị rớt cùng lúc. Cần để nó ngừng nhận kết nối mới, đợi vài giây cho kết nối cũ xong rồi mới tắt, mỗi lần đợi một thời gian hơi khác nhau | Server | N |
| 2 | Ghi nhật ký dễ đọc | Đổi nhật ký sang dạng có cấu trúc để lọc theo camera và tìm lỗi dễ hơn. Vẫn không ghi mật khẩu hay nội dung nhạy cảm | Server | N |
| 3 | Bảng theo dõi sức khoẻ | Có trang số liệu (số camera online, số lần từ chối, độ trễ) để biết hệ thống khoẻ hay yếu. Hiện chỉ có trang đếm số kết nối | Server | V |
| 4 | Cất mật khẩu và khoá đúng chỗ | Cấu hình nhạy cảm đọc từ biến môi trường hoặc kho bí mật, không để trong tham số dòng lệnh | Server | N |
| 5 | Tự gia hạn chứng chỉ ngay trong chương trình | Để dịch vụ chỉ cần **một cổng** (mục 2.1): chương trình tự xin và gia hạn chứng chỉ trên cổng 443, bỏ được nginx và cổng 80 | Server | V |
| 6 | Camera nối lại thông minh | Mất kết nối thì chờ lâu dần (10, 30, 60, 300 giây) và có chút ngẫu nhiên, thay vì luôn 30 giây. Tránh hàng nghìn camera dồn cùng lúc khi server vừa lên lại. SID bị thu hồi thì ngừng hẳn | Camera | N |
| 7 | Bớt bộ nhớ cho mỗi kết nối | Giảm hàng đợi tin nhắn mỗi kết nối từ 256 xuống khoảng 32 và đặt trần bộ nhớ cho chương trình. Tăng sức chứa khoảng 10%, giữ máy không bị đầy (xem mục 8) | Server | N |

### Nhóm 2 — Mở rộng

| # | Việc | Giải thích đơn giản | Ai làm | Cỡ |
|---|---|---|---|---|
| 8 | Danh sách TURN dùng chung | TURN là trạm chuyển tiếp video khi camera và điện thoại không nối thẳng được, chạy riêng trên máy khác. Hiện chỉ có một trạm cố định đọc từ tập tin. Cần danh sách nhiều trạm do chương trình chính cấp, tài khoản tạm thời cho từng phiên, và TURN qua TLS cho phía điện thoại | Server + App | V |
| 9 | App tự chọn trạm gần nhất | Server chỉ đưa danh sách (TURN và server). App tự đo trạm nào phản hồi nhanh nhất rồi chọn | App | V |
| 10 | Nhiều server | Hiện camera chỉ nối được đúng một địa chỉ cố định. Cho phép camera được chỉ sang server khác. Gợi ý: chọn server bằng công thức từ mã camera, nên không cần cơ sở dữ liệu chung để nhớ "camera nào ở server nào" | Server + Camera | L |
| 11 | Camera tự giới hạn người xem | Số người xem cùng lúc và thời gian tối đa một phiên do camera tự kiểm soát. Server không theo dõi từng phiên | Camera + App | V |

### Nhóm 3 — Để sau

| # | Việc | Ghi chú | Ai làm | Cỡ |
|---|---|---|---|---|
| 12 | Camera ở Trung Quốc | Tìm địa chỉ server qua DNS riêng của Trung Quốc. Đã chốt để sau | Camera + App | N |
| 13 | Nơi lưu dùng chung | Chỉ cần khi có nhiều server trung tâm. Hiện là một tập tin trên một máy | Server | L |

Đã bỏ: việc "cầu nối SID cũ/mới" (hai hệ thống độc lập, mục 6).

## 4. Việc cố ý KHÔNG làm

| Tính năng của dịch vụ cũ | Vì sao không làm |
| --- | --- |
| Server chuyển tiếp video (relay ghép cặp) | Video đi thẳng giữa hai đầu, chỉ khi bất khả kháng mới qua TURN |
| Giới hạn 15 phút và đếm phiên trên server | Giao cho camera/app (việc 9). Server đơn giản hơn |
| Server chọn trạm theo vùng và tải | Giao cho app tự đo (việc 6) |
| Ghim thiết bị thử nghiệm cho QA | Đội test dùng bản app trỏ vào server thử, không cần cơ chế riêng |
| Làm rối dữ liệu tự chế | WebRTC đã mã hoá chuẩn, mạnh hơn |
| Đo loại NAT | Đã có TURN |
| Kênh gộp nhiều dịch vụ (AIO) và chuyển tiếp cổng TCP tổng quát | Dùng kênh dữ liệu JSON có sẵn. Chỉ làm nếu sau này cần gọi API camera kiểu cũ |
| Hai biến thể đóng gói (flavor) | Không áp dụng |
| PROXY protocol, cổng TLS cho relay | Không có relay. Đã có tuỳ chọn chạy sau bộ cân bằng tải |

## 5. Chạy song song với dịch vụ cũ trên VPS thật

Quyết định: hai dịch vụ **độc lập hoàn toàn**, không dùng chung dữ liệu (MySQL, Redis, tập tin), không liên quan gì về SID. Camera nào chạy chương trình nào là tuỳ từng camera nên không cần kế hoạch riêng cho camera.

### 5.1 VPS hiện tại (kiểm tra ngày 2026-10-19, chỉ đọc)

| Mục | Hiện trạng |
| --- | --- |
| Cấu hình máy | 2 vCPU, **RAM 939 MB, chỉ còn khoảng 185 MB trống, không có swap**; ổ đĩa còn 7,8 GB; tải đang gần 0 |
| Dịch vụ mới (PM2) | Bản "production" nghe cổng **443**; bản dev nghe cổng **8443** |
| Các thứ khác trên máy | nginx (cổng **80**, chỉ lo gia hạn chứng chỉ cho `webrtc.5gen.care`), TURN Violet (UDP 3478), mediamtx, **Postgres trong Docker (cổng 5432, có vẻ của dự án khác)**, một dịch vụ thống kê NAT (cổng 59980), cloudflared |
| Dịch vụ cũ (vrelay) | **Chưa cài**: không có cấu hình, thư mục hay chương trình nào |
| Giới hạn số kết nối mở | 524288, đủ |

### 5.2 Điều này có nghĩa gì

| Chủ đề | Tác động | Đề xuất |
| --- | --- | --- |
| **Trùng cổng 443 và 80** | Camera và app của dịch vụ cũ có sẵn mặc định nối vào **443** (điều khiển) và **80** (relay phía app). Hai cổng này đang do dịch vụ mới và nginx giữ. Các cổng khác của dịch vụ cũ (8800, 8801, 9901–9904, 81, 8810, 6060) đang trống | Vì dịch vụ mới **chưa rollout** và cổng của nó chỉnh được, nên nhường 443 và 80 cho dịch vụ cũ, dịch vụ mới chuyển sang cổng khác (bản dev đã dùng 8443). Việc này cần đổi cấu hình camera thử và phần gia hạn chứng chỉ (xem dòng dưới) |
| Gia hạn chứng chỉ | nginx đang giữ cổng 80 để xác thực chứng chỉ cho tên miền của dịch vụ mới. Nếu nhường cổng 80, cách này không dùng được nữa | Chuyển sang xác thực qua DNS, hoặc dùng cổng 80 cho nginx và đặt relay cũ ở cổng khác (81) khi thử |
| **RAM rất ít** | Chỉ còn khoảng 185 MB trống, không có swap. Dịch vụ cũ cần thêm bộ nhớ (chương trình Go, và MySQL hoặc Redis). **MySQL không nên cài trên máy này** | Chạy dịch vụ cũ với Redis nhỏ (hoặc chỉ Redis, tắt MySQL như spec cho phép), hoặc dùng máy riêng cho bản thử. Thêm swap nếu bắt buộc chạy chung. Chưa có số đo bộ nhớ của dịch vụ cũ nên cần đo |
| Máy dùng chung với dự án khác | Postgres và nattypedb trên máy là của việc khác | Không đụng vào. Đặt giới hạn bộ nhớ/CPU cho cả hai dịch vụ thử để không kéo chúng chậm |
| Cổng 5432 (Postgres) | Đang nghe trên mọi địa chỉ. Có bị lộ ra Internet hay không tuỳ cấu hình tường lửa của VPS, mình không xem được | Ngoài phạm vi dự án này, nhưng nên kiểm tra |
| Tranh tài nguyên khi đo | Dịch vụ cũ chuyển tiếp **toàn bộ video** qua máy, dịch vụ mới chủ yếu không (trừ khi qua TURN). Với 2 vCPU, hai bên ảnh hưởng nhau | Đo từng bên riêng trước, rồi mới đo cùng lúc. Trong lúc so sánh, hạn chế dùng TURN trên cùng máy |
| Khởi động lại | Vì chưa rollout nên rủi ro thấp, nhưng khởi động lại một dịch vụ vẫn không nên làm trong lúc đo bên kia | Báo trước |
| Dữ liệu | Hoàn toàn tách biệt: dịch vụ cũ ghi MySQL/Redis của nó, dịch vụ mới ghi tập tin của nó. Không thể ghi đè nhau | Giữ như vậy |

## 6. Quyết định đã chốt (2026-10-19) và việc còn mở

| # | Câu hỏi | Quyết định |
| --- | --- | --- |
| A | SID cũ và SID mới | **Hai hệ thống chạy song song, độc lập.** Không cần cầu nối. Việc 11 bỏ |
| B | Dùng chung dữ liệu "camera ở server nào" | **Không dùng chung.** Hai dịch vụ độc lập |
| C | Thứ tự | **Vận hành trước** (việc 1–4), rồi mở rộng |
| D | Camera Trung Quốc | **Để sau.** Việc 7 chuyển xuống cuối |
| E | Cổng trên VPS | Đã kiểm tra (mục 5.1). Chưa rollout nên có thể điều chỉnh cổng. Camera thử không cần lập kế hoạch |

Việc còn mở:

| # | Câu hỏi | Gợi ý |
| --- | --- | --- |
| F | Dịch vụ cũ có chạy trên chính VPS này không, hay trên máy thử riêng? | RAM chỉ còn 185 MB và không có swap, nên máy riêng an toàn hơn. Nếu chạy chung thì cần thêm swap và đo trước |
| G | Nếu chạy chung: dịch vụ nào nhận cổng 443 và 80? | Dịch vụ cũ nhận (vì camera và app cũ có sẵn cổng mặc định), dịch vụ mới chuyển sang 8443 |

## 7. Thuật ngữ

| Từ | Nghĩa đơn giản |
| --- | --- |
| Signaling (tổng đài) | Server giúp camera và điện thoại tìm nhau và thoả thuận cách nối. Không chở video |
| WebRTC | Công nghệ nối thẳng hai thiết bị để truyền video |
| TURN | Trạm trung chuyển khi hai thiết bị không nối thẳng được (ví dụ do mạng công ty) |
| SID | Mã cấp phép của camera |
| Tenant | Một khách hàng/nhóm. Người xem của nhóm này không thấy camera nhóm khác |
| Center | Máy chủ kiểm tra SID và cấp vé vào server tín hiệu |
| Graceful shutdown | Tắt máy chủ một cách êm, không làm rớt đột ngột |
| Node | Một máy chủ trong hệ thống nhiều máy |

## 8. Ước tính sức chứa trên VPS hiện có

**Giả định:** VPS chỉ chạy dịch vụ này (2 vCPU, RAM 939 MB). Mỗi camera luôn online, mỗi camera có một app xem 30 phút mỗi ngày. TURN chạy ở máy khác nên không tính ở đây.

**Số đo thật (ngày 2026-10-19):** mình chạy chính `signaling-server` (bản hiện tại, HTTPS với chứng chỉ RSA 2048) trên máy mình, khoá vào 2 nhân CPU, rồi mở 2.000, 10.000 và 20.000 kết nối WebSocket thật.

| Số kết nối | RAM chương trình dùng | RAM mỗi kết nối | CPU server |
| --- | --- | --- | --- |
| 2.000 | 117 MB | khoảng 55 KB | 4 giây |
| 10.000 | 519 MB | khoảng 51 KB | 21 giây |
| 20.000 | 1.019 MB | khoảng 51 KB | 41 giây |

Tốc độ nhận kết nối mới đạt khoảng 950 kết nối mỗi giây (bao gồm bắt tay TLS), tốn khoảng 2 mili-giây CPU mỗi kết nối.

**Cách tính:**
- Số kết nối cùng lúc = số camera + số người đang xem. 30 phút mỗi ngày là khoảng 2% thời gian. Nếu giờ cao điểm đông gấp 5 lần thì khoảng 10% camera có người xem cùng lúc. Kết nối cùng lúc khoảng **1,1 × số camera**.
- RAM dùng được cho dịch vụ khoảng 600 MB (máy 939 MB, trừ hệ điều hành và PM2 khoảng 150–300 MB). Cộng thêm khoảng 10% cho bộ nhớ của hệ điều hành mỗi kết nối (không nằm trong số đo trên) thì mỗi kết nối tốn khoảng 57 KB. Chừa 30% dự phòng để không bị đầy khi có đợt tăng đột ngột.

| RAM dùng được | Trần kết nối | Kết nối an toàn (chừa 30%) | Số camera phục vụ được |
| --- | --- | --- | --- |
| 500 MB | khoảng 8.800 | khoảng 6.100 | khoảng **5.500** |
| 600 MB | khoảng 10.500 | khoảng 7.400 | khoảng **6.700** |
| 700 MB | khoảng 12.300 | khoảng 8.600 | khoảng **7.800** |

**Kết luận:** nên lập kế hoạch cho khoảng **6.000 camera** trên VPS này, mỗi camera một người xem 30 phút mỗi ngày. Trần tuyệt đối khoảng 9.000–10.000 camera nếu chấp nhận rủi ro đầy bộ nhớ. Việc 7 (bớt bộ nhớ mỗi kết nối) tăng thêm khoảng 10%. Muốn nhiều hơn thì giới hạn là **RAM, không phải CPU hay mạng**: mỗi 1 GB RAM thêm khoảng 15.000 camera.

**Những thứ không phải là giới hạn:**
- CPU lúc bình thường: nhịp ping 30 giây của 6.000 kết nối chỉ khoảng 200 tin nhắn mỗi giây.
- Mạng của server tín hiệu: vài chục KB mỗi giây. Video không đi qua đây (đi thẳng giữa hai đầu hoặc qua TURN ở máy khác).
- Số phiên xem: 6.000 phiên mỗi ngày, mỗi phiên vài KB tin nhắn.

**Điểm nghẽn thật là lúc khởi động lại:** nếu server restart, cả 6.000 camera nối lại cùng lúc, mỗi camera cần hai lần bắt tay TLS (một với Center, một cho WebSocket). Với khoảng 500–900 bắt tay mỗi giây thì mất khoảng 15–25 giây nếu camera nối lại ngay lập tức. Việc 1 và việc 6 giảm rủi ro này.

**Độ tin cậy của con số:** đây là ước tính từ máy mình (loopback, kết nối rảnh, chưa có vé xác thực, CPU khác với VPS). RAM mỗi kết nối chắc chắn hơn CPU. Trước khi rollout cần chạy lại bài đo trên chính VPS.

