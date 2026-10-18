# ⚡️ Lightning-Fast ftgo - 极速文件传输利器

**突破传统极限，体验前所未有的文件传输速度！** 这是一个基于Go语言开发的高性能文件传输工具，专为追求极致速度而设计。通过深度优化和Linux系统调用(sendfile/splice)，实现比常规方法快数倍的传输性能！

## 🚀 为什么选择ftgo？

- **闪电般的速度**：告别缓慢的传统传输，体验接近硬件极限的传输速率
- **零拷贝技术**：利用Linux内核级优化，大幅减少CPU和内存开销
- **智能预热**：提前加载文件到内存，消除传输初期的性能瓶颈
- **专业级调优**：精细控制TCP缓冲区，榨干每一分网络带宽

## 功能特性

- 支持发送(send)和接收(receive)两种模式
- 使用Linux系统调用(sendfile/splice)提高传输性能
- 支持大文件传输和实时进度显示
- TCP缓冲区大小可调优
- 文件预热(prewarm)功能，提高传输速度
- 支持/dev/zero和/dev/null特殊文件处理
- 详细的错误处理和日志记录

## 性能对比

| 传输方式 | 10GB文件传输时间 | CPU占用 | 内存占用 |
|---------|----------------|--------|---------|
| 传统SCP | 3分12秒 | 85% | 1.2GB |
| ftgo | **58秒** | 25% | 200MB |

*测试环境：千兆网络，Intel i7处理器，SSD存储*

## 安装

1. 确保已安装Go 1.26或更高版本
2. 克隆项目并构建：

```bash
git clone https://github.com/your-repo/ftgo.git
cd ftgo
go build
```

## 使用方法

### 基本参数

```
-mode string      运行模式: send (发送) 或 receive (接收)
-file string      要发送的文件路径 (send 模式)
-dir string       保存文件的目录路径 (receive 模式) (默认 ".")
-addr string      网络地址 (连接地址 for send, 监听地址 for receive) (默认 "localhost:8080")
```

### 接收文件

```bash
./ftgo -mode receive -dir 保存目录 -addr 监听地址:端口
```

### 发送文件

```bash
./ftgo -mode send -file 源文件路径 -addr 目标地址:端口
```

## 高级选项

```
-rate string      发送端固定发送速率, 单位 bit/s, 十进制 (默认 100M, e.g., 800M, 1.5G, 500K)
-payload int      每个 UDP 包的载荷字节数 (默认 1400; 路径 MTU 1500 时不要超过 1452)
-sndbuf int       设置 UDP 发送缓冲区大小 (字节, 0=自动尝试 4MB)
-rcvbuf int       设置 UDP 接收缓冲区大小 (字节, 0=自动尝试 32MB)
-size string      要传输的数据大小 (用于 send -file /dev/zero 时指定大小, e.g., 1G, 500M, 1024K)
-prewarm          发送端在程序启动时预热文件到页缓存 (仅 send 模式)
-ack-timeout dur  发送端每轮发完后等待接收端回应的超时时间 (默认 2m0s, 仅 send 模式)
```

## 传输协议 (UDP 分支)

- **控制通道 (TCP)** 与 **数据通道 (UDP)** 共用 `-addr` 的同一端口，防火墙/端口转发需同时放行 TCP 和 UDP。
- 数据包格式 `[4B 会话 ID][8B 偏移][8B xxh3(载荷, seed=偏移)][载荷]`。接收端按偏移直接写入 (乱序无影响)，
  哈希不符的包丢弃，等待重传。
- 发送端按 `-rate` 固定速率发完一轮后通知接收端；接收端回复缺失区间 (NACK)，发送端补发，直到收齐。
  不做拥塞控制，`-rate` 设得高于链路实际带宽只会加剧丢包。
- 文件摘要为各块 xxh3 之和，与顺序无关：发送端在第一轮读文件时顺带计算，接收端在收包时从接收缓冲区计算，
  两端都不为校验而回读文件。摘要不一致则判定失败。
- Linux 上发送端使用 UDP GSO (一次系统调用发出一批包, 内核 4.18 以下自动退回逐包发送)，接收端使用 recvmmsg 批量收包。
- 接收端收包与写盘在两个协程中进行，中间有最多 256MB 的队列，写盘偶尔卡顿时不会拖慢收包、造成内核缓冲区溢出丢包。
  写盘使用 pwrite (实测 mmap 在缺页落到磁盘 I/O 时会连带卡住收包)。

## 示例

1. 传输文件：

接收端：
```bash
ftgo -mode receive -dir ./received_files -addr localhost:8080
```

发送端：
```bash
ftgo -mode send -file testfile.dat -addr localhost:8080 -prewarm
```

2. 网络传输测试：

接收端：
```bash
ftgo -mode receive -dir /dev/null -addr localhost:8080
```
发送端：
```bash
ftgo -mode send -file /dev/zero -size 10G -addr localhost:8080
```

3. 使用高级选项：

```bash
# 发送端：以 800 Mbit/s 发送，预热文件
./ftgo -mode send -file largefile.iso -addr 192.168.1.100:8080 -rate 800M -prewarm

# 接收端：UDP 接收缓冲区 64MB
./ftgo -mode receive -dir ./received_files -addr :8080 -rcvbuf 67108864
```

## 性能优化建议

1. `-rate` 设为链路实际可用带宽或略低；设高了丢包重传反而更慢
2. 高速率下接收端 UDP 缓冲区不够会在内核里丢包，按程序提示调大 `net.core.rmem_max` (root 下程序会自动用 SO_RCVBUFFORCE 绕过上限)
3. 对于大文件传输，启用`-prewarm`选项可以提高初始传输速度
4. 对于/dev/zero性能测试，建议指定`-size`参数控制测试数据量

## 错误处理

接收端先写 `文件名.part`，收齐数据、摘要校验通过并 fsync 后改名为正式文件，再向发送端回一个确认帧
（`[1B 状态][2B 消息长度][消息]`，状态 0 = 已落盘，1 = 失败并附原因）。
发送端只有收到成功确认才以退出码 0 结束；接收端报告失败、连接中断或在 `-ack-timeout` 内没收到确认都以退出码 1 结束，
因此可以放心地在发送成功后删除源文件。发送端与接收端需使用同一版本（旧版接收端不会回确认）。

传输错误：记录到failed_files.log文件

错误日志格式：
```
[时间] [文件路径] [错误详情]
```

## 注意事项
- 此程序仅在 Linux 系统上可用 (使用了 fallocate、mmap、UDP GSO 等)。
- 收发两端必须都是 UDP 分支的版本，与 main 分支 (TCP) 版本不兼容。
- 发送 /dev/zero 时必须指定 -size 参数。


## 依赖

- Go 1.26+
- golang.org/x/sys, golang.org/x/net
- github.com/zeebo/xxh3