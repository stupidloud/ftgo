# ftgo - 高延迟链路上的快速文件传输

ftgo 是一个 Go 编写的点对点文件传输工具。数据走 UDP，按指定速率固定发送，丢包靠 NACK 重传补齐，
不受 TCP 拥塞控制在高延迟、有丢包链路上"越丢越慢"的影响，适合跨洲、跨运营商等长距离传输。

## 特点

- **固定速率发送**：`-rate` 指定速率，RTT 再高也能跑满带宽
- **可靠**：丢包自动重传；每个包带 xxh3 校验，整个文件再做一次摘要比对
- **校验不回读文件**：摘要在发送和接收数据时顺带算出，两端都不为校验再读一遍文件
- **安全落盘**：先写 `.part` 临时文件，校验通过并 fsync 后才改名，发送端收到接收端确认才算成功
- **批量收发**：发送端 UDP GSO，接收端 recvmmsg；收包与写盘分在两个协程，写盘卡顿不会造成丢包

## 实测

s9 (欧洲, 1 Gbit/s 网卡) → s25 (美国)，RTT 122ms，4GB 随机文件写入接收端磁盘：

| 方式 | 速度 | 说明 |
|------|------|------|
| ftgo 旧版 (TCP) | 约 155 Mbit/s | 高 RTT 下单条 TCP 流的窗口涨不上去 |
| iperf3 TCP 8 并发 | 738 Mbit/s | 参考 |
| **ftgo (UDP)，`-rate 800M`** | **约 690 Mbit/s 端到端** | 0 重传，含最后 fsync，sha256 一致 |
| ftgo (UDP)，`-rate 900M` | 约 850 Mbit/s | 2GB `/dev/zero`，重传 0.01% |

`-rate` 设得超过链路实际带宽 (如上例的 1G) 只会增加丢包重传，总耗时不会缩短。

## 安装

从 [Releases](https://github.com/stupidloud/ftgo/releases) 下载 `ftgo_linux_amd64` / `ftgo_linux_arm64` (静态编译，无依赖)，或自行构建 (需要 Go 1.26+)：

```bash
git clone https://github.com/stupidloud/ftgo.git
cd ftgo
CGO_ENABLED=0 go build
```

## 使用方法

接收端：
```bash
./ftgo -mode receive -dir ./received_files -addr :8080
```

发送端：
```bash
./ftgo -mode send -file largefile.iso -addr 192.168.1.100:8080 -rate 800M
```

网络测试 (不读写磁盘)：
```bash
./ftgo -mode receive -dir /dev/null -addr :8080
./ftgo -mode send -file /dev/zero -size 10G -addr 192.168.1.100:8080 -rate 1G
```

### 参数

```
-mode string      运行模式: send (发送) 或 receive (接收) (默认 "send")
-file string      要发送的文件路径 (send 模式)
-dir string       保存文件的目录路径, /dev/null 表示丢弃 (receive 模式) (默认 ".")
-addr string      连接地址 (send) / 监听地址 (receive), TCP 与 UDP 共用此端口 (默认 "localhost:8080")
-rate string      固定发送速率, 单位 bit/s, 十进制 (默认 100M, e.g., 800M, 1.5G, 500K)
-payload int      每个 UDP 包的载荷字节数 (默认 1400; 路径 MTU 1500 时不要超过 1452)
-sndbuf int       UDP 发送缓冲区大小 (字节, 0=自动尝试 4MB)
-rcvbuf int       UDP 接收缓冲区大小 (字节, 0=自动尝试 32MB)
-size string      发送 /dev/zero 时的数据大小 (e.g., 1G, 500M, 1024K)
-prewarm          发送前预读文件到页缓存
-ack-timeout dur  发送端每轮发完后等待接收端回应的超时时间 (默认 2m0s)
```

### 调优建议

1. `-rate` 设为链路实际可用带宽或略低。可以先用 `/dev/zero` → `/dev/null` 试几个速率，看重传比例。
2. 接收端 UDP 缓冲区不够时程序会提示调大 `net.core.rmem_max`；以 root 运行时会自动用 SO_RCVBUFFORCE 绕过上限。
3. 路径上有隧道/PPPoE 等使 MTU 变小时，相应调小 `-payload`。

## 工作原理

- **控制通道 (TCP)** 与 **数据通道 (UDP)** 共用 `-addr` 的同一端口，防火墙/端口转发需同时放行 TCP 和 UDP。
- 数据包格式 `[4B 会话 ID][8B 偏移][8B xxh3(载荷, seed=偏移)][载荷]`。接收端按偏移直接写入，乱序无影响；
  哈希不符的包丢弃，等待重传。
- 发送端按 `-rate` 发完一轮后通知接收端；接收端回复缺失区间 (NACK)，发送端补发，直到收齐。
  不做拥塞控制。
- 文件摘要为各块 xxh3 之和，与顺序无关：发送端第一轮读文件时顺带计算，接收端收包时从接收缓冲区计算。
- 发送端使用 UDP GSO (一次系统调用发出一批包，内核 4.18 以下自动退回逐包发送)，接收端使用 recvmmsg 批量收包。
- 接收端收包与写盘在两个协程中进行，中间有最多 256MB 的队列，写盘偶尔卡顿时不会拖慢收包、造成内核缓冲区溢出丢包。
- 一个接收端同一时间只处理一个传输。多个发送端同时连接时按到达顺序排队，发送端会显示"前面还有 N 个传输"，
  不限时一直等到轮到为止。固定速率不做拥塞控制，并发传输只会互相挤占带宽，所以不做并发。

## 错误处理

接收端先写 `文件名.part`，收齐数据、摘要校验通过并 fsync 后改名为正式文件，再向发送端回确认。
发送端只有收到成功确认才以退出码 0 结束；接收端报告失败、连接中断或在 `-ack-timeout` 内没收到回应都以退出码 1 结束，
因此可以放心地在发送成功后删除源文件。

读文件出错或接收端未确认的传输会记入发送端当前目录的 `failed_files.log`：
```
[时间] - [文件路径] - [错误详情]
```

## 注意事项

- 仅支持 Linux。
- 收发两端必须使用相同协议的版本：v0.1.0 起为 UDP 协议，与 v0.0.x (TCP) 版本不兼容。
- 发送 /dev/zero 时必须指定 `-size`。

## 依赖

- golang.org/x/sys, golang.org/x/net
- github.com/zeebo/xxh3
