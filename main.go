package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeebo/xxh3"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix" // Use unix package instead of syscall (preferred for Linux)
)

func formatWithCommas(n int64) string {
	s := strconv.FormatInt(n, 10)
	length := len(s)
	if length <= 3 {
		return s
	}
	firstCommaPos := length % 3
	if firstCommaPos == 0 {
		firstCommaPos = 3
	}
	var result strings.Builder
	result.WriteString(s[:firstCommaPos])
	for i := firstCommaPos; i < length; i += 3 {
		result.WriteString(",")
		result.WriteString(s[i : i+3])
	}
	return result.String()
}

const (
	udpHeaderSize    = 20 // UDP 数据包头: [4B 会话 ID][8B 偏移][8B xxh3(载荷, seed=偏移)]
	udpOverhead      = 28 // IPv4 + UDP 头, 计入发送速率
	maxPayloadSize   = 65507 - udpHeaderSize
	readBlockSize    = 1 << 20               // 发送端每次 pread 的块大小 (按载荷大小取整)
	maxGSOSegments   = 64                    // 内核 UDP_MAX_SEGMENTS
	maxGSOBytes      = 65000                 // 一次 GSO 写入的总长上限 (UDP 长度字段 16 位)
	recvBatchSize    = 64                    // 接收端一次 recvmmsg 最多收的包数
	recvQueueBytes   = 256 << 20             // 收包协程与写盘协程之间队列的最大内存
	quietPeriod      = 50 * time.Millisecond // 接收端收到轮次结束后, UDP 静默这么久才统计缺失
	maxNackRanges    = 8192                  // 单个 NACK 最多携带的缺失区间数, 其余留到下一轮
	maxStallRounds   = 10                    // 连续这么多轮一个新块都没收到则判定失败
	defaultUDPRcvBuf = 32 << 20
	defaultUDPSndBuf = 4 << 20
)

var (
	mode    = flag.String("mode", "send", "运行模式: send (连接并发送) 或 receive (监听并接收)") // 恢复模式说明
	file    = flag.String("file", "", "要发送的文件路径 (send 模式)")                       // 发送端仍需指定文件
	dir     = flag.String("dir", ".", "保存文件的目录路径 (receive 模式)")                   // 接收端指定目录
	addr    = flag.String("addr", "localhost:8080", "网络地址 (连接地址 for send, 监听地址 for receive; TCP 控制通道与 UDP 数据通道共用此端口)")
	badFile = "failed_files.log" // 记录传输失败的文件
	sndBuf  = flag.Int("sndbuf", 0, "设置 UDP 发送缓冲区大小 (字节, 0=自动尝试 4MB)")
	rcvBuf  = flag.Int("rcvbuf", 0, "设置 UDP 接收缓冲区大小 (字节, 0=自动尝试 32MB)")
	sizeStr = flag.String("size", "", "要传输的数据大小 (用于 send -file /dev/zero 时指定大小, e.g., 1G, 500M, 1024K)") // 更新 size 说明
	prewarm = flag.Bool("prewarm", false, "发送端在程序启动时预热文件到页缓存 (仅 send 模式)")
	ackWait = flag.Duration("ack-timeout", 120*time.Second, "发送端每轮发完后等待接收端回应的超时时间 (send 模式)")
	rateStr = flag.String("rate", "100M", "发送端固定发送速率, 单位 bit/s, 十进制 (e.g., 800M, 1.5G, 500K) (send 模式)")
	payload = flag.Int("payload", 1400, "每个 UDP 包的载荷字节数 (send 模式; 路径 MTU 1500 时不要超过 1452)")
)

// 控制通道 (TCP) 消息, 首字节为类型。
// 接收端 → 发送端:
//
//	ackOK / ackFail / msgReady: [1B 类型][2B 消息长度][消息]
//	msgNack:                    [1B 类型][4B 区间数][区间数 × (8B 起始块号, 4B 块数)]
//
// 发送端 → 接收端 (文件头之后):
//
//	msgRoundEnd: [1B 类型][8B 摘要] 本轮 UDP 数据已全部发出
const (
	ackOK       byte = 0 // 文件已校验、fsync 并改名为正式文件
	ackFail     byte = 1 // 失败, 消息为原因
	msgReady    byte = 2 // 接收端已打开文件, 可以开始发 UDP
	msgNack     byte = 3
	msgRoundEnd byte = 4
)

// chunkRange 是一段连续的块号 [first, first+count)
type chunkRange struct {
	first int64
	count int64
}

// AckError 表示发送端没有拿到接收端的成功确认 (接收端报告失败, 或确认超时/连接断开)
type AckError struct {
	Err error
}

func (e *AckError) Error() string {
	return e.Err.Error()
}

func (e *AckError) Unwrap() error {
	return e.Err
}

type ReadIOError struct {
	FilePath string
	Offset   int64
	Err      error
}

func (e *ReadIOError) Error() string {
	return fmt.Sprintf("文件 '%s' 在偏移量 %d 处读取失败: %v", e.FilePath, e.Offset, e.Err)
}

func (e *ReadIOError) Unwrap() error {
	return e.Err
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "使用方法: ftgo [参数]\n")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\n示例:")
		fmt.Fprintln(os.Stderr, "  发送本地文件:")
		fmt.Fprintln(os.Stderr, "    ftgo -mode receive -dir ./received_files -addr localhost:8080")
		fmt.Fprintln(os.Stderr, "    ftgo -mode send -file testfile.dat -addr localhost:8080 -rate 800M -prewarm")
		fmt.Fprintln(os.Stderr, "  传输性能测试示例:")
		fmt.Fprintln(os.Stderr, "    接收端: ftgo -mode receive -dir /dev/null -addr localhost:8080")
		fmt.Fprintln(os.Stderr, "    发送端: ftgo -mode send -file /dev/zero -size 10G -addr localhost:8080 -rate 1G")
		fmt.Fprintln(os.Stderr, "\n注意事项:")
		fmt.Fprintln(os.Stderr, "  - 此程序仅在 Linux 系统上可用。")
		fmt.Fprintln(os.Stderr, "  - 数据走 UDP, 控制走 TCP, 两者使用 -addr 的同一端口, 防火墙/端口转发需同时放行。")
		fmt.Fprintln(os.Stderr, "  - 发送速率固定为 -rate, 不做拥塞控制; 设得高于链路实际带宽只会加剧丢包。")
		fmt.Fprintln(os.Stderr, "  - 发送 /dev/zero 时必须指定 -size 参数。")
	}
	flag.Parse()

	if runtime.GOOS != "linux" {
		log.Fatal("错误: 此程序只能在Linux系统上运行")
	}

	if len(os.Args) == 1 {
		flag.Usage()
		os.Exit(0)
	}

	if *mode == "send" {
		if *file == "" {
			log.Fatal("错误: send 模式下必须指定 -file 参数")
		}
		if *file == "/dev/zero" && *sizeStr == "" {
			log.Fatal("错误: 使用 -file /dev/zero 时必须指定 -size 参数")
		}
		if *file == "/dev/zero" {
			if _, err := parseSize(*sizeStr); err != nil {
				log.Fatalf("错误: 使用 -file /dev/zero 时 -size 参数无效: %v", err)
			}
		}
		if _, err := parseRate(*rateStr); err != nil {
			log.Fatalf("错误: -rate 参数无效: %v", err)
		}
		if *payload < 64 || *payload > maxPayloadSize {
			log.Fatalf("错误: -payload 必须在 64 到 %d 之间", maxPayloadSize)
		}
	}
	if *mode == "receive" && *dir == "" {
		log.Fatal("错误: receive 模式下必须指定 -dir 参数")
	}

	if *mode == "send" && *prewarm && *file != "/dev/zero" {
		if err := doPrewarm(*file); err != nil {
			// Prewarming failure is logged as a warning but doesn't stop the process
			log.Printf("\x1b[33m警告: 文件预热失败: %v\x1b[0m", err)
		}
	}

	switch *mode {
	case "send":
		if err := sender(*file, *addr); err != nil {
			log.Printf("\x1b[31m发送失败: %v\x1b[0m", err)
			// 传输开始后的失败 (读文件出错、接收端未确认) 记入失败日志, 便于重传
			var ackErr *AckError
			var readErr *ReadIOError
			if errors.As(err, &ackErr) || errors.As(err, &readErr) {
				logFailedFile(*file, err.Error())
			}
			os.Exit(1)
		}
		fmt.Println("文件发送成功完成.")
	case "receive":
		err := receiver(*dir, *addr) // receiver handles /dev/null internally
		if err != nil {
			log.Fatalf("\x1b[31m接收端错误: %v\x1b[0m", err)
		}

	default:
		log.Fatalf("错误: 无效的模式 %q. 请使用 'send' 或 'receive'", *mode) // 恢复默认错误消息
	}
}

// logFailedFile records details about failed transfers.
func logFailedFile(filePath string, reason string) {
	f, err := os.OpenFile(badFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("错误: 无法打开失败日志文件 %s: %v", badFile, err)
		return
	}
	defer f.Close()
	logLine := fmt.Sprintf("%s - %s - %s\n", time.Now().Format(time.RFC3339), filePath, reason)
	if _, err := f.WriteString(logLine); err != nil {
		log.Printf("错误: 写入失败日志文件 %s 失败: %v", badFile, err)
	}
}

func displayProgress(totalSize int64, transferred *int64, startTime time.Time, done chan struct{}) {
	show := func(end string) {
		current := atomic.LoadInt64(transferred)
		elapsed := max(time.Since(startTime).Seconds(), 0.1)
		progress := 100.0
		if totalSize > 0 {
			progress = min(float64(current)*100/float64(totalSize), 100)
		}
		fmt.Printf("\r\033[K进度: %.2f%% (%s/%s bytes), 速度: %.2f MB/s%s", progress,
			formatWithCommas(current), formatWithCommas(totalSize), float64(current)/elapsed/1024/1024, end)
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	show("")
	for {
		select {
		case <-ticker.C:
			show("")
		case <-done:
			show("\n")
			return
		}
	}
}

// ctrlMsg 是从控制通道读到的一条消息 (err 非空表示连接已出错)
type ctrlMsg struct {
	typ    byte
	text   string
	ranges []chunkRange
	err    error
}

// readCtrlLoop 持续读取接收端发来的控制消息, 直到连接出错或 stop 关闭
func readCtrlLoop(conn net.Conn, out chan<- ctrlMsg, stop <-chan struct{}) {
	for {
		m := readCtrl(conn)
		select {
		case out <- m:
		case <-stop:
			return
		}
		if m.err != nil {
			return
		}
	}
}

func readCtrl(conn net.Conn) ctrlMsg {
	typ := make([]byte, 1)
	if _, err := io.ReadFull(conn, typ); err != nil {
		return ctrlMsg{err: err}
	}
	m := ctrlMsg{typ: typ[0]}
	switch m.typ {
	case ackOK, ackFail, msgReady:
		lenBytes := make([]byte, 2)
		if _, err := io.ReadFull(conn, lenBytes); err != nil {
			return ctrlMsg{err: err}
		}
		text := make([]byte, binary.BigEndian.Uint16(lenBytes))
		if _, err := io.ReadFull(conn, text); err != nil {
			return ctrlMsg{err: err}
		}
		m.text = string(text)
	case msgNack:
		cnt := make([]byte, 4)
		if _, err := io.ReadFull(conn, cnt); err != nil {
			return ctrlMsg{err: err}
		}
		n := binary.BigEndian.Uint32(cnt)
		if n > maxNackRanges {
			return ctrlMsg{err: fmt.Errorf("NACK 区间数 %d 超过上限", n)}
		}
		body := make([]byte, int(n)*12)
		if _, err := io.ReadFull(conn, body); err != nil {
			return ctrlMsg{err: err}
		}
		m.ranges = make([]chunkRange, n)
		for i := range m.ranges {
			m.ranges[i].first = int64(binary.BigEndian.Uint64(body[i*12:]))
			m.ranges[i].count = int64(binary.BigEndian.Uint32(body[i*12+8:]))
		}
	default:
		return ctrlMsg{err: fmt.Errorf("未知控制消息类型 %d", m.typ)}
	}
	return m
}

// waitCtrl 等待下一条控制消息; 超时或连接出错返回 *AckError
func waitCtrl(ch <-chan ctrlMsg, timeout time.Duration) (ctrlMsg, error) {
	select {
	case m := <-ch:
		if m.err != nil {
			return m, &AckError{Err: fmt.Errorf("控制连接中断: %w", m.err)}
		}
		return m, nil
	case <-time.After(timeout):
		return ctrlMsg{}, &AckError{Err: fmt.Errorf("等待接收端回应超时 (%v)", timeout)}
	}
}

func sender(filePath string, connectAddr string) error {
	isDevZero := (filePath == "/dev/zero")
	rate, err := parseRate(*rateStr)
	if err != nil {
		return err
	}
	chunkSize := int64(*payload)

	var fileSize int64
	var fileName string
	var srcFile *os.File // nil 表示 /dev/zero

	if isDevZero {
		fileSize, err = parseSize(*sizeStr) // 从 -size 获取大小
		if err != nil {
			// 理论上 main 函数已检查，但再次检查更安全
			return fmt.Errorf("无法解析 -size 参数 '%s' 用于 /dev/zero: %w", *sizeStr, err)
		}
		fileName = "zero.dat" // 给 /dev/zero 一个虚拟文件名
		log.Printf("发送 /dev/zero，虚拟文件名: %s, 大小: %d bytes", fileName, fileSize)
	} else {
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			return fmt.Errorf("文件 '%s' 处理失败: %w", filePath, err)
		}
		fileSize = fileInfo.Size()
		fileName = fileInfo.Name() // 获取真实文件名
		srcFile, err = os.Open(filePath)
		if err != nil {
			return fmt.Errorf("打开源文件 '%s' 失败: %w", filePath, err)
		}
		defer srcFile.Close()
		unix.Fadvise(int(srcFile.Fd()), 0, 0, unix.FADV_SEQUENTIAL)
	}

	conn, err := net.DialTimeout("tcp", connectAddr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("连接失败 %s: %w", connectAddr, err)
	}
	defer conn.Close()
	log.Printf("\x1b[32m已连接到接收端 %s\x1b[0m", connectAddr)

	// 1. 文件头: [2B 文件名长度][文件名][8B 文件大小][4B 会话 ID][2B 载荷大小]
	session := rand.Uint32()
	hdr := binary.BigEndian.AppendUint16(nil, uint16(len(fileName)))
	hdr = append(hdr, fileName...)
	hdr = binary.BigEndian.AppendUint64(hdr, uint64(fileSize))
	hdr = binary.BigEndian.AppendUint32(hdr, session)
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(chunkSize))
	if _, err := conn.Write(hdr); err != nil {
		return fmt.Errorf("发送文件头失败: %w", err)
	}
	log.Printf("\x1b[32m已发送文件头: %s, %s bytes, 会话 %08x, 载荷 %d bytes\x1b[0m", fileName, formatWithCommas(fileSize), session, chunkSize)

	ctrl := make(chan ctrlMsg, 1)
	stopCtrl := make(chan struct{})
	defer close(stopCtrl)
	go readCtrlLoop(conn, ctrl, stopCtrl)

	// 2. 等接收端就绪 (已打开文件、准备好收 UDP)
	m, err := waitCtrl(ctrl, 30*time.Second)
	if err != nil {
		return err
	}
	if m.typ == ackFail {
		return &AckError{Err: fmt.Errorf("接收端拒绝: %s", m.text)}
	}
	if m.typ != msgReady {
		return &AckError{Err: fmt.Errorf("期望就绪消息, 收到类型 %d", m.typ)}
	}

	// 3. UDP 数据通道: 与 TCP 连接的对端 IP、端口相同
	tcpRemote := conn.RemoteAddr().(*net.TCPAddr)
	udpConn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: tcpRemote.IP, Port: tcpRemote.Port, Zone: tcpRemote.Zone})
	if err != nil {
		return fmt.Errorf("建立 UDP 数据通道失败: %w", err)
	}
	defer udpConn.Close()
	setUDPBuffer(udpConn, *sndBuf, defaultUDPSndBuf, false)

	nChunks := (fileSize + chunkSize - 1) / chunkSize
	blockChunks := max(int64(1), readBlockSize/chunkSize)
	segSize := int(udpHeaderSize + chunkSize)
	// 每批最多攒约 1ms 的数据量一次发出, 既减少系统调用又不至于突发过大
	maxSegs := min(maxGSOSegments, maxGSOBytes/segSize, max(1, int(rate/8/1000)/(segSize+udpOverhead)))
	s := &udpSender{
		conn:      udpConn,
		src:       srcFile,
		filePath:  filePath,
		fileSize:  fileSize,
		chunkSize: chunkSize,
		session:   session,
		pacer:     pacer{bytesPerSec: rate / 8},
		block:     make([]byte, blockChunks*chunkSize),
		batch:     make([]byte, maxSegs*segSize),
		maxSegs:   maxSegs,
		gso:       enableGSO(udpConn, segSize),
		ctrl:      ctrl,
	}
	sendMode := "逐包发送 (GSO 不可用)"
	if s.gso {
		sendMode = fmt.Sprintf("GSO, 每批最多 %d 个包", maxSegs)
	}
	log.Printf("UDP 数据通道 -> %s, 固定速率 %s, 共 %s 个数据包, %s", udpConn.RemoteAddr(), formatRate(rate), formatWithCommas(nChunks), sendMode)

	var transferred int64
	startTime := time.Now()
	done := make(chan struct{})
	go displayProgress(fileSize, &transferred, startTime, done)

	// 4. 第一轮: 按顺序发出全部数据, 同时计算摘要 (之后不再为摘要读文件)
	digest, err := s.sendRanges([]chunkRange{{0, nChunks}}, &transferred)
	close(done)
	time.Sleep(50 * time.Millisecond) // 让进度显示输出最后一行
	if err != nil {
		return err
	}
	log.Printf("\x1b[32m第 1 轮发送完成, 耗时 %v, 摘要 %016x\x1b[0m", time.Since(startTime).Round(time.Millisecond), digest)

	// 5. 轮次循环: 通知本轮结束 → 接收端回缺失区间 → 补发, 直到接收端确认
	var retransChunks int64
	roundEnd := make([]byte, 9)
	roundEnd[0] = msgRoundEnd
	binary.BigEndian.PutUint64(roundEnd[1:], digest)
	for round := 2; ; round++ {
		if _, err := conn.Write(roundEnd); err != nil {
			return &AckError{Err: fmt.Errorf("发送轮次结束消息失败: %w", err)}
		}
		m, err := waitCtrl(ctrl, *ackWait)
		if err != nil {
			return err
		}
		switch m.typ {
		case ackOK:
			log.Printf("发送完成，接收端已校验并保存，总共 %s bytes, 用时 %v, 重传 %s 个包 (%.2f%%)",
				formatWithCommas(fileSize), time.Since(startTime).Round(time.Millisecond),
				formatWithCommas(retransChunks), float64(retransChunks)*100/float64(max(nChunks, 1)))
			return nil
		case ackFail:
			return &AckError{Err: fmt.Errorf("接收端保存失败: %s", m.text)}
		case msgNack:
			var n int64
			for _, r := range m.ranges {
				if r.first < 0 || r.count <= 0 || r.first+r.count > nChunks {
					return &AckError{Err: fmt.Errorf("接收端请求了无效的块区间 [%d, +%d)", r.first, r.count)}
				}
				n += r.count
			}
			log.Printf("第 %d 轮: 重传 %s 个包 (%d 个区间)", round, formatWithCommas(n), len(m.ranges))
			if _, err := s.sendRanges(m.ranges, nil); err != nil {
				return err
			}
			retransChunks += n
		default:
			return &AckError{Err: fmt.Errorf("意外的控制消息类型 %d", m.typ)}
		}
	}
}

// pacer 把发送速率限制在固定值: 超前超过 1ms 就睡, 落后太多则重新计时而不是突发追赶
type pacer struct {
	bytesPerSec float64
	start       time.Time
	sent        int64
}

func (p *pacer) reset() {
	p.start = time.Now()
	p.sent = 0
}

func (p *pacer) wait(n int) {
	p.sent += int64(n)
	due := p.start.Add(time.Duration(float64(p.sent) / p.bytesPerSec * float64(time.Second)))
	d := time.Until(due)
	if d > time.Millisecond {
		time.Sleep(d)
	} else if d < -20*time.Millisecond {
		p.reset()
	}
}

type udpSender struct {
	conn      *net.UDPConn
	src       *os.File // nil 表示 /dev/zero, 数据全零
	filePath  string
	fileSize  int64
	chunkSize int64
	session   uint32
	pacer     pacer
	block     []byte // pread 缓冲区, 大小为载荷的整数倍
	batch     []byte // 待发出的一批包, 首尾相接 (GSO 时由内核按包长切分)
	batchLen  int
	batchPkts int
	maxSegs   int
	gso       bool
	ctrl      <-chan ctrlMsg
}

// enableGSO 在 socket 上开启 UDP_SEGMENT: 之后一次 write 的缓冲区会被内核按 segSize 切成多个 UDP 包
func enableGSO(c *net.UDPConn, segSize int) bool {
	rc, err := c.SyscallConn()
	if err != nil {
		return false
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_UDP, unix.UDP_SEGMENT, segSize)
	}); err != nil {
		return false
	}
	return serr == nil
}

// sendRanges 按速率发出指定区间的块, 返回这些块哈希之和 (第一轮即为文件摘要)。
// progress 非空时累加已发出的载荷字节数。
func (s *udpSender) sendRanges(ranges []chunkRange, progress *int64) (uint64, error) {
	var digest uint64
	blockChunks := int64(len(s.block)) / s.chunkSize
	s.pacer.reset()
	for _, r := range ranges {
		end := r.first + r.count
		for idx := r.first; idx < end; idx += blockChunks {
			// 接收端出错时会提前回失败确认, 不必把整轮发完
			select {
			case m := <-s.ctrl:
				if m.err != nil {
					return 0, &AckError{Err: fmt.Errorf("发送中控制连接中断: %w", m.err)}
				}
				if m.typ == ackFail {
					return 0, &AckError{Err: fmt.Errorf("接收端保存失败: %s", m.text)}
				}
				return 0, &AckError{Err: fmt.Errorf("发送中收到意外的控制消息类型 %d", m.typ)}
			default:
			}

			off := idx * s.chunkSize
			n := min(min(blockChunks, end-idx)*s.chunkSize, s.fileSize-off)
			data := s.block[:n]
			if s.src != nil {
				if _, err := s.src.ReadAt(data, off); err != nil {
					return 0, &ReadIOError{FilePath: s.filePath, Offset: off, Err: err}
				}
			}
			for p := int64(0); p < n; p += s.chunkSize {
				chunk := data[p:min(p+s.chunkSize, n)]
				h, err := s.queueChunk(off+p, chunk)
				if err != nil {
					return 0, err
				}
				digest += h
				if progress != nil {
					atomic.AddInt64(progress, int64(len(chunk)))
				}
			}
			if err := s.flush(); err != nil {
				return 0, err
			}
		}
	}
	return digest, nil
}

// queueChunk 把一个块封包追加到当前批次, 批次满或遇到短块 (文件最后一块, GSO 要求它在批末) 时发出
func (s *udpSender) queueChunk(off int64, data []byte) (uint64, error) {
	h := xxh3.HashSeed(data, uint64(off))
	pkt := s.batch[s.batchLen : s.batchLen+udpHeaderSize+len(data)]
	binary.BigEndian.PutUint32(pkt[0:4], s.session)
	binary.BigEndian.PutUint64(pkt[4:12], uint64(off))
	binary.BigEndian.PutUint64(pkt[12:20], h)
	copy(pkt[udpHeaderSize:], data)
	s.batchLen += len(pkt)
	s.batchPkts++
	if s.batchPkts == s.maxSegs || int64(len(data)) < s.chunkSize {
		return h, s.flush()
	}
	return h, nil
}

func (s *udpSender) flush() error {
	if s.batchPkts == 0 {
		return nil
	}
	buf := s.batch[:s.batchLen]
	if s.gso {
		err := s.write(buf)
		if errors.Is(err, unix.EIO) { // 网卡不支持校验和卸载时 GSO 会报 EIO, 退回逐包发送
			log.Printf("\x1b[33m警告: GSO 发送失败 (%v), 改为逐包发送\x1b[0m", err)
			s.gso = false
		} else if err != nil {
			return err
		}
	}
	if !s.gso {
		segSize := udpHeaderSize + int(s.chunkSize)
		for p := 0; p < len(buf); p += segSize {
			if err := s.write(buf[p:min(p+segSize, len(buf))]); err != nil {
				return err
			}
		}
	}
	s.pacer.wait(len(buf) + s.batchPkts*udpOverhead)
	s.batchLen, s.batchPkts = 0, 0
	return nil
}

func (s *udpSender) write(buf []byte) error {
	for {
		_, err := s.conn.Write(buf)
		if err == nil {
			return nil
		}
		if errors.Is(err, unix.ENOBUFS) { // 网卡队列满, 稍后重试
			time.Sleep(100 * time.Microsecond)
			continue
		}
		if errors.Is(err, unix.ECONNREFUSED) {
			return fmt.Errorf("接收端 UDP 端口不可达 (收到 ICMP port unreachable): %w", err)
		}
		if errors.Is(err, unix.EIO) {
			return err
		}
		return fmt.Errorf("UDP 发送失败: %w", err)
	}
}

// setUDPBuffer 设置 UDP socket 缓冲区: 先尝试无视 rmem_max/wmem_max 上限的 *BUFFORCE (需 root),
// 失败再用普通选项, 最后读回实际值, 不足时提示调整 sysctl。
func setUDPBuffer(c *net.UDPConn, want, fallback int, recv bool) {
	if want <= 0 {
		want = fallback
	}
	opt, force, name, sysctl := unix.SO_SNDBUF, unix.SO_SNDBUFFORCE, "发送", "net.core.wmem_max"
	if recv {
		opt, force, name, sysctl = unix.SO_RCVBUF, unix.SO_RCVBUFFORCE, "接收", "net.core.rmem_max"
	}
	rc, err := c.SyscallConn()
	if err != nil {
		log.Printf("\x1b[33m警告: 设置 UDP %s缓冲区失败: %v\x1b[0m", name, err)
		return
	}
	var got int
	rc.Control(func(fd uintptr) {
		if unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, force, want) != nil {
			unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, opt, want)
		}
		got, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, opt)
	})
	got /= 2 // 内核返回的是加倍后的值
	if got < want {
		log.Printf("\x1b[33m警告: UDP %s缓冲区只有 %s bytes (期望 %s), 高速率下可能丢包; 可执行 sysctl -w %s=%d\x1b[0m",
			name, formatWithCommas(int64(got)), formatWithCommas(int64(want)), sysctl, want)
	} else {
		log.Printf("UDP %s缓冲区: %s bytes", name, formatWithCommas(int64(got)))
	}
}

// --- 文件预热函数 (使用 Readahead 预热整个文件) ---
func doPrewarm(filePath string) error {
	log.Printf("发起预读请求: 文件 %s (整个文件)...", filePath)
	prewarmStartTime := time.Now()

	// 获取文件信息以得到大小
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("预热时获取文件信息失败: %w", err)
	}
	fileSize := fileInfo.Size()
	if fileSize == 0 {
		log.Printf("文件 %s 大小为 0，跳过预热。", filePath)
		return nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("预热时打开文件失败: %w", err)
	}
	defer f.Close()
	fd := int(f.Fd())
	_, _, err = unix.Syscall(unix.SYS_READAHEAD, uintptr(fd), uintptr(0), uintptr(fileSize))

	prewarmDuration := time.Since(prewarmStartTime)

	// unix.Syscall 返回的 err 是 unix.Errno 类型
	// 如果 err == 0, 表示成功
	if err.(unix.Errno) != 0 { // Errno 应与 0 比较而非 nil
		// Readahead 失败通常不是致命的，记录警告
		log.Printf("\x1b[33m警告: 发起 Readahead 请求失败: %v\x1b[0m", err)
	} else {
		log.Printf("Readahead 请求已发起，耗时: %v (注意: 数据加载是异步的)", prewarmDuration)
	}
	// Readahead 失败不应阻止主流程
	return nil
}

func receiver(dirPath string, listenAddr string) error {
	// 添加变量来跟踪所有文件的传输统计
	var totalBytesReceived int64
	var totalFilesReceived int
	var startTime = time.Now()

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", listenAddr, err) // 监听失败是致命错误
	}
	defer listener.Close()

	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("解析 UDP 地址 %s 失败: %w", listenAddr, err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("监听 UDP %s 失败: %w", listenAddr, err)
	}
	defer udpConn.Close()
	setUDPBuffer(udpConn, *rcvBuf, defaultUDPRcvBuf, true)
	log.Printf("\x1b[32m服务器启动，正在监听 %s (TCP 控制 + UDP 数据)\x1b[0m", listenAddr)

	for { // 无限循环，顺序处理连接
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("\x1b[33m警告: 接受连接失败: %v\x1b[0m", err)
			// 检查是否是监听器关闭导致的错误
			if errors.Is(err, net.ErrClosed) {
				log.Println("监听器已关闭，服务器退出。")
				return nil // 正常退出
			}
			continue // 其他接受错误，继续等待下一个连接
		}

		remoteAddrStr := conn.RemoteAddr().String()
		log.Printf("[%s] 接收到连接，开始处理...", remoteAddrStr)
		connectionStart := time.Now()

		fileName, fileTransferred, fileReceiveError := receiveFile(conn, udpConn, dirPath)

		if fileTransferred > 0 && fileReceiveError == nil {
			elapsed := time.Since(connectionStart).Seconds()
			fileAvgSpeed := float64(fileTransferred) / elapsed / 1024 / 1024
			log.Printf("\x1b[32m[%s] 传输完成，文件 '%s' 接收了 %s bytes，速度: %.2f MB/s\x1b[0m",
				remoteAddrStr, fileName, formatWithCommas(fileTransferred), fileAvgSpeed)

			totalBytesReceived += fileTransferred
			totalFilesReceived++
		}

		totalElapsed := time.Since(startTime).Seconds()
		if totalBytesReceived > 0 && totalElapsed > 0 {
			overallAvgSpeed := float64(totalBytesReceived) / totalElapsed / 1024 / 1024
			log.Printf("\x1b[32m累计接收: %d 个文件，总大小 %s bytes，平均速度: %.2f MB/s\x1b[0m",
				totalFilesReceived, formatWithCommas(totalBytesReceived), overallAvgSpeed)
		}

		log.Printf("等待下一个连接...")
	}
}

// receiveFile 处理一个发送端连接: 读文件头、收 UDP 数据、按轮次回 NACK, 最后校验并回确认
func receiveFile(conn net.Conn, udpConn *net.UDPConn, dirPath string) (fileName string, totalReceived int64, receiveErr error) {
	defer conn.Close()
	remoteAddrStr := conn.RemoteAddr().String()

	var targetPath string // 实际写入的路径 (常规文件时为临时 .part 文件)
	var finalPath string  // 传输成功后改名到的正式路径
	var useTempFile bool  // 是否启用临时文件 + 原子改名 (常规文件为 true, /dev/null 为 false)

	// 错误处理和清理
	defer func() {
		if receiveErr != nil {
			log.Printf("\x1b[31m[%s] 错误: %v\x1b[0m", remoteAddrStr, receiveErr)
		}
		// 临时文件收尾 (此时 dstFile 已由更晚注册的 defer 关闭):
		// 成功则原子改名为正式文件, 失败则删除残缺临时文件, 避免留下/覆盖为不完整文件
		if useTempFile {
			if receiveErr == nil {
				if err := os.Rename(targetPath, finalPath); err != nil {
					receiveErr = fmt.Errorf("重命名临时文件 '%s' -> '%s' 失败: %w", targetPath, finalPath, err)
					log.Printf("\x1b[31m[%s] 错误: %v\x1b[0m", remoteAddrStr, receiveErr)
					os.Remove(targetPath)
				} else if err := syncDir(dirPath); err != nil {
					// 改名已生效, 目录项未落盘只记警告, 不判失败
					log.Printf("\x1b[33m[%s] 警告: 同步目录 '%s' 失败: %v\x1b[0m", remoteAddrStr, dirPath, err)
				}
			} else {
				if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
					log.Printf("\x1b[33m[%s] 警告: 删除残缺临时文件 '%s' 失败: %v\x1b[0m", remoteAddrStr, targetPath, err)
				} else {
					log.Printf("[%s] 已删除残缺临时文件 '%s'", remoteAddrStr, targetPath)
				}
			}
		}
		// 告诉发送端结果: 只有文件已校验、fsync 并改名成功才回成功
		sendFrame(conn, ackOK, receiveErr)
		log.Printf("[%s] 连接已关闭", remoteAddrStr)
	}()

	// 1. 文件头: [2B 文件名长度][文件名][8B 文件大小][4B 会话 ID][2B 载荷大小]
	lenBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, lenBytes); err != nil {
		return "", 0, fmt.Errorf("读取文件名长度失败: %w", err)
	}
	fileNameBytes := make([]byte, binary.BigEndian.Uint16(lenBytes))
	if _, err := io.ReadFull(conn, fileNameBytes); err != nil {
		return "", 0, fmt.Errorf("读取文件名失败: %w", err)
	}
	fileName = string(fileNameBytes)
	rest := make([]byte, 14)
	if _, err := io.ReadFull(conn, rest); err != nil {
		return fileName, 0, fmt.Errorf("读取文件头失败: %w", err)
	}
	fileSize := int64(binary.BigEndian.Uint64(rest[0:8]))
	session := binary.BigEndian.Uint32(rest[8:12])
	chunkSize := int64(binary.BigEndian.Uint16(rest[12:14]))
	if fileSize < 0 || chunkSize < 64 || chunkSize > maxPayloadSize {
		return fileName, 0, fmt.Errorf("文件头无效: 大小 %d, 载荷 %d", fileSize, chunkSize)
	}
	log.Printf("\x1b[32m[%s] 文件: %s, %s 字节, 会话 %08x, 载荷 %d\x1b[0m", remoteAddrStr, fileName, formatWithCommas(fileSize), session, chunkSize)

	// 检查目标是否为 /dev/null，并设置 targetPath
	isDevNull := (dirPath == "/dev/null")
	var dstFile *os.File
	var write func(off int64, p []byte) error
	if isDevNull {
		log.Printf("\x1b[32m[%s] 数据将被丢弃 (/dev/null)\x1b[0m", remoteAddrStr)
	} else {
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			return fileName, 0, fmt.Errorf("创建目录 '%s' 失败: %w", dirPath, err)
		}
		finalPath = filepath.Join(dirPath, fileName)
		targetPath = finalPath + ".part" // 先写临时文件, 全部成功后再原子改名
		useTempFile = true
		log.Printf("\x1b[32m[%s] 将文件 '%s' 保存到: %s (临时文件: %s)\x1b[0m", remoteAddrStr, fileName, finalPath, targetPath)

		var err error
		dstFile, err = os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
		if err != nil {
			return fileName, 0, fmt.Errorf("创建/打开目标文件 '%s' 失败: %w", targetPath, err)
		}
		defer dstFile.Close()

		if fileSize > 0 {
			// 预分配: 磁盘空间不足时在开始传输前就失败, 也减少乱序写入造成的碎片
			fallocErr := unix.Fallocate(int(dstFile.Fd()), 0, 0, fileSize)
			if errors.Is(fallocErr, unix.ENOSPC) {
				return fileName, 0, fmt.Errorf("磁盘空间不足, 无法预分配 %s bytes", formatWithCommas(fileSize))
			}
			if fallocErr != nil {
				log.Printf("\x1b[33m[%s] 警告: 预分配文件空间失败: %v\x1b[0m", remoteAddrStr, fallocErr)
			}
		}
		// 用 pwrite 而不是 mmap: 实测 mmap 缺页落到磁盘 I/O 时会连带卡住收包协程, 造成成批丢包
		write = func(off int64, p []byte) error {
			_, err := dstFile.WriteAt(p, off)
			return err
		}
	}

	// 2. 开始收 UDP, 然后通知发送端就绪
	nChunks := (fileSize + chunkSize - 1) / chunkSize
	var transferred int64
	rx := &udpReceiver{
		conn:      udpConn,
		session:   session,
		fileSize:  fileSize,
		chunkSize: chunkSize,
		nChunks:   nChunks,
		write:     write,
		got:       make([]uint64, (nChunks+63)/64),
		progress:  &transferred,
		fatal:     make(chan error, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go rx.run()
	defer func() {
		close(rx.stop)
		<-rx.done
	}()

	rcvbufErrsStart := udpRcvbufErrors()
	sendFrame(conn, msgReady, nil)

	startTime := time.Now()
	done := make(chan struct{})
	go displayProgress(fileSize, &transferred, startTime, done)
	stopProgress := sync.OnceFunc(func() {
		close(done)
		time.Sleep(50 * time.Millisecond)
	})
	defer stopProgress()

	// 控制通道读取放到单独 goroutine, 以便同时响应 UDP 收包协程的致命错误
	roundEnds := make(chan uint64)
	ctrlErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 9)
		for {
			if _, err := io.ReadFull(conn, buf); err != nil {
				ctrlErr <- err
				return
			}
			if buf[0] != msgRoundEnd {
				ctrlErr <- fmt.Errorf("意外的控制消息类型 %d", buf[0])
				return
			}
			select {
			case roundEnds <- binary.BigEndian.Uint64(buf[1:]):
			case <-rx.stop:
				return
			}
		}
	}()

	// 3. 轮次循环
	var lastReceived int64
	stall := 0
	for round := 1; ; round++ {
		var senderDigest uint64
		select {
		case senderDigest = <-roundEnds:
		case err := <-ctrlErr:
			return fileName, rx.receivedBytes(), fmt.Errorf("控制连接中断: %w", err)
		case err := <-rx.fatal:
			return fileName, rx.receivedBytes(), err
		}

		rx.waitQuiet()
		select {
		case err := <-rx.fatal:
			return fileName, rx.receivedBytes(), err
		default:
		}

		ranges, missing, received, digest := rx.missing(maxNackRanges)
		if missing == 0 {
			stopProgress()
			if digest != senderDigest {
				return fileName, rx.receivedBytes(), fmt.Errorf("数据校验失败: 发送端摘要 %016x, 接收端摘要 %016x", senderDigest, digest)
			}
			log.Printf("\x1b[32m[%s] 全部 %s 个包已收齐, 摘要 %016x 校验通过, 共 %d 轮, 丢弃校验失败的包 %d 个\x1b[0m",
				remoteAddrStr, formatWithCommas(nChunks), digest, round, rx.bad.Load())
			log.Printf("[%s] 内核 UDP 接收缓冲区溢出丢包 %d 个 (全机计数), 写盘队列最大积压 %d 批, 队列满等待 %d 次",
				remoteAddrStr, udpRcvbufErrors()-rcvbufErrsStart, rx.peakQueue.Load(), rx.queueFull.Load())
			break
		}

		if received == 0 {
			return fileName, 0, fmt.Errorf("没有收到任何 UDP 数据包, 请检查防火墙/端口转发是否放行 UDP %s", udpConn.LocalAddr())
		}
		if received == lastReceived {
			stall++
			if stall >= maxStallRounds {
				return fileName, rx.receivedBytes(), fmt.Errorf("连续 %d 轮没有收到新数据, 仍缺 %s 个包", stall, formatWithCommas(missing))
			}
		} else {
			stall = 0
		}
		lastReceived = received

		log.Printf("[%s] 第 %d 轮结束: 缺失 %s 个包 (%.2f%%), 请求重传 %d 个区间",
			remoteAddrStr, round, formatWithCommas(missing), float64(missing)*100/float64(nChunks), len(ranges))
		if err := sendNack(conn, ranges); err != nil {
			return fileName, rx.receivedBytes(), fmt.Errorf("发送 NACK 失败: %w", err)
		}
	}

	// 4. 回确认前先把数据刷到磁盘, 否则确认后断电仍会丢文件 (发送端可能已删除源文件)
	if useTempFile {
		if err := dstFile.Sync(); err != nil {
			return fileName, rx.receivedBytes(), fmt.Errorf("同步文件 '%s' 到磁盘失败: %w", targetPath, err)
		}
	}
	return fileName, rx.receivedBytes(), nil
}

// udpReceiver 在单独的 goroutine 中收取一个会话的 UDP 数据包, 用位图记录已收到的块
type udpReceiver struct {
	conn      *net.UDPConn
	session   uint32
	fileSize  int64
	chunkSize int64
	nChunks   int64
	write     func(off int64, p []byte) error // nil 表示丢弃 (/dev/null)
	progress  *int64

	mu       sync.Mutex
	got      []uint64 // 已收到的块位图
	received int64    // 已收到的块数
	digest   uint64   // 已收到块的哈希之和

	bad       atomic.Int64 // 哈希/长度校验失败而丢弃的包
	lastRecv  atomic.Int64 // 最近一次收到包的时间 (UnixNano)
	pending   atomic.Int64 // 已从 socket 收下、尚未被写盘协程处理的包数
	queueFull atomic.Int64 // 收包协程因队列满而等待写盘协程的次数
	peakQueue atomic.Int64 // 队列中积压的最大批次数

	fatal chan error
	stop  chan struct{}
	done  chan struct{}
}

// batchReader 是 ipv4/ipv6.PacketConn 的 ReadBatch (Linux 上为 recvmmsg)
type batchReader interface {
	ReadBatch(ms []ipv4.Message, flags int) (int, error)
}

// rxBatch 是收包协程交给写盘协程的一批包
type rxBatch struct {
	msgs []ipv4.Message
	n    int
}

// run 是收包协程: 只负责把包从 socket 里尽快取出, 校验和写盘交给 writeLoop,
// 这样写盘偶尔卡顿 (页缓存回写、缺页) 时由用户态队列吸收, 不至于让内核 socket 缓冲区溢出丢包。
func (r *udpReceiver) run() {
	defer close(r.done)
	var pc batchReader = ipv4.NewPacketConn(r.conn)
	if a, ok := r.conn.LocalAddr().(*net.UDPAddr); ok && a.IP.To4() == nil {
		pc = ipv6.NewPacketConn(r.conn)
	}

	segSize := udpHeaderSize + int(r.chunkSize)
	maxSlots := max(4, recvQueueBytes/(recvBatchSize*segSize))
	free := make(chan *rxBatch, maxSlots)
	full := make(chan *rxBatch, maxSlots)
	allocated := 0
	newBatch := func() *rxBatch {
		buf := make([]byte, recvBatchSize*segSize)
		b := &rxBatch{msgs: make([]ipv4.Message, recvBatchSize)}
		for i := range b.msgs {
			b.msgs[i].Buffers = [][]byte{buf[i*segSize : (i+1)*segSize]}
		}
		allocated++
		return b
	}

	writerDone := make(chan struct{})
	go r.writeLoop(full, free, writerDone)
	defer func() {
		close(full)
		<-writerDone
	}()

	push := func(b *rxBatch) {
		full <- b
		if q := int64(len(full)); q > r.peakQueue.Load() {
			r.peakQueue.Store(q)
		}
	}

	var cur *rxBatch
	// 读超时用来定期检查 stop 和交出未满的批次, 到期才重设, 不在每次读之前调用 SetReadDeadline
	r.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	for {
		if cur == nil {
			// 队列内存按需分配, 用满后才阻塞等写盘协程归还
			select {
			case cur = <-free:
			default:
				if allocated < maxSlots {
					cur = newBatch()
				} else {
					r.queueFull.Add(1)
					select {
					case cur = <-free:
					case <-r.stop:
						return
					}
				}
			}
			cur.n = 0
		}
		n, err := pc.ReadBatch(cur.msgs[cur.n:], 0)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if cur.n > 0 {
					push(cur)
					cur = nil
				}
				select {
				case <-r.stop:
					return
				default:
				}
				r.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				continue
			}
			r.fatal <- fmt.Errorf("UDP 接收失败: %w", err)
			return
		}
		cur.n += n
		r.pending.Add(int64(n))
		r.lastRecv.Store(time.Now().UnixNano())
		// 写盘协程空闲时立即交出; 它忙时继续把本批攒满, 让队列容量按字节而不是按批次计
		if cur.n == len(cur.msgs) || len(full) == 0 {
			push(cur)
			cur = nil
		}
	}
}

// writeLoop 是写盘协程: 校验、记录位图并写入文件, 处理完把批次缓冲还给收包协程
func (r *udpReceiver) writeLoop(full <-chan *rxBatch, free chan<- *rxBatch, done chan<- struct{}) {
	defer close(done)
	failed := false
	for b := range full {
		if !failed {
			if err := r.handleBatch(b.msgs[:b.n]); err != nil {
				r.fatal <- err
				failed = true // 继续取空队列, 让收包协程不被阻塞
			}
		}
		r.pending.Add(-int64(b.n))
		free <- b
	}
}

func (r *udpReceiver) handleBatch(msgs []ipv4.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range msgs {
		if err := r.handle(m.Buffers[0][:m.N]); err != nil {
			return err
		}
	}
	return nil
}

// handle 处理一个包 (调用方持有 r.mu)
func (r *udpReceiver) handle(pkt []byte) error {
	if len(pkt) < udpHeaderSize || binary.BigEndian.Uint32(pkt[0:4]) != r.session {
		return nil // 其他会话的残留包
	}
	off := int64(binary.BigEndian.Uint64(pkt[4:12]))
	h := binary.BigEndian.Uint64(pkt[12:20])
	data := pkt[udpHeaderSize:]
	if off < 0 || off >= r.fileSize || off%r.chunkSize != 0 ||
		int64(len(data)) != min(r.chunkSize, r.fileSize-off) ||
		xxh3.HashSeed(data, uint64(off)) != h {
		r.bad.Add(1)
		return nil
	}

	idx := off / r.chunkSize
	if r.got[idx/64]&(1<<(idx%64)) != 0 {
		return nil // 重复包
	}
	if r.write != nil {
		if err := r.write(off, data); err != nil {
			return fmt.Errorf("在偏移量 %d 写入失败: %w", off, err)
		}
	}
	r.got[idx/64] |= 1 << (idx % 64)
	r.received++
	r.digest += h
	atomic.AddInt64(r.progress, int64(len(data)))
	return nil
}

// waitQuiet 等到 UDP 静默 quietPeriod 且队列里的包都已写完,
// 让轮次结束消息之前发出、仍在路上、在 socket 缓冲区或用户态队列里的包先处理完
func (r *udpReceiver) waitQuiet() {
	deadline := time.Now().Add(60 * time.Second)
	start := time.Now().UnixNano()
	for time.Now().Before(deadline) {
		last := max(r.lastRecv.Load(), start)
		if time.Since(time.Unix(0, last)) >= quietPeriod && r.pending.Load() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// udpRcvbufErrors 读取内核累计的 UDP 接收缓冲区溢出丢包数 (全机所有 socket), 读不到返回 -1
func udpRcvbufErrors() int64 {
	data, err := os.ReadFile("/proc/net/snmp")
	if err != nil {
		return -1
	}
	var names []string
	for line := range strings.Lines(string(data)) {
		if !strings.HasPrefix(line, "Udp:") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if names == nil {
			names = fields
			continue
		}
		for i, name := range names {
			if name == "RcvbufErrors" && i < len(fields) {
				v, _ := strconv.ParseInt(fields[i], 10, 64)
				return v
			}
		}
	}
	return -1
}

// missing 返回最多 limit 个缺失区间, 以及缺失总块数、已收块数和当前摘要
func (r *udpReceiver) missing(limit int) (ranges []chunkRange, missing, received int64, digest uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := int64(0); i < r.nChunks; {
		w := r.got[i/64]
		if w == math.MaxUint64 {
			i = (i/64 + 1) * 64
			continue
		}
		if w&(1<<(i%64)) != 0 {
			i++
			continue
		}
		start := i
		for i < r.nChunks && r.got[i/64]&(1<<(i%64)) == 0 {
			i++
		}
		missing += i - start
		for s := start; s < i && len(ranges) < limit; s += math.MaxUint32 {
			ranges = append(ranges, chunkRange{s, min(i-s, math.MaxUint32)})
		}
	}
	return ranges, missing, r.received, r.digest
}

func (r *udpReceiver) receivedBytes() int64 {
	return atomic.LoadInt64(r.progress)
}

func sendNack(conn net.Conn, ranges []chunkRange) error {
	buf := make([]byte, 5, 5+len(ranges)*12)
	buf[0] = msgNack
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(ranges)))
	for _, r := range ranges {
		buf = binary.BigEndian.AppendUint64(buf, uint64(r.first))
		buf = binary.BigEndian.AppendUint32(buf, uint32(r.count))
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Write(buf)
	return err
}

// sendFrame 向发送端回写 [1B 类型][2B 长度][消息] 帧; recvErr 非空时改为失败确认。写失败 (对方已断开) 只记日志
func sendFrame(conn net.Conn, typ byte, recvErr error) {
	msg := ""
	if recvErr != nil {
		typ, msg = ackFail, recvErr.Error()
		if len(msg) > 65535 {
			msg = msg[:65535]
		}
	}
	frame := make([]byte, 3+len(msg))
	frame[0] = typ
	binary.BigEndian.PutUint16(frame[1:3], uint16(len(msg)))
	copy(frame[3:], msg)
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(frame); err != nil {
		log.Printf("\x1b[33m[%s] 警告: 回写控制消息失败: %v\x1b[0m", conn.RemoteAddr(), err)
	}
}

// syncDir 让目录项 (改名结果) 落盘
func syncDir(dirPath string) error {
	d, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// parseSize 解析带单位的大小字符串 (如 "1G", "500M", "1024K") 返回字节数
// (strconv 和 strings 已在顶部导入)

func parseSize(sizeStr string) (int64, error) {
	sizeStr = strings.ToUpper(strings.TrimSpace(sizeStr))
	multiplier := int64(1)
	suffix := ""

	if strings.HasSuffix(sizeStr, "G") {
		multiplier = 1024 * 1024 * 1024
		suffix = "G"
	} else if strings.HasSuffix(sizeStr, "M") {
		multiplier = 1024 * 1024
		suffix = "M"
	} else if strings.HasSuffix(sizeStr, "K") {
		multiplier = 1024
		suffix = "K"
	}

	numStr := strings.TrimSuffix(sizeStr, suffix)
	num, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析数字部分 '%s': %w", numStr, err)
	}

	if num <= 0 {
		return 0, fmt.Errorf("大小必须为正数")
	}

	return num * multiplier, nil
}

// parseRate 解析速率字符串 (如 "800M", "1.5G", "500K"), 单位 bit/s, 十进制倍数
func parseRate(rateStr string) (float64, error) {
	s := strings.ToUpper(strings.TrimSpace(rateStr))
	multiplier := 1.0
	switch {
	case strings.HasSuffix(s, "G"):
		multiplier, s = 1e9, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		multiplier, s = 1e6, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		multiplier, s = 1e3, strings.TrimSuffix(s, "K")
	}
	num, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析数字部分 '%s': %w", s, err)
	}
	if num <= 0 || math.IsInf(num, 0) || math.IsNaN(num) {
		return 0, fmt.Errorf("速率必须为正数")
	}
	return num * multiplier, nil
}

func formatRate(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.2f Gbit/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.2f Mbit/s", bps/1e6)
	default:
		return fmt.Sprintf("%.2f Kbit/s", bps/1e3)
	}
}
