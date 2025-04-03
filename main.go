package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv" // 为 parseSize 添加
	"strings" // 为 parseSize 添加
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix" // 使用 unix 包代替 syscall
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
	copyBufferSize = 65536 // 用于 io.CopyBuffer 和 splice 的缓冲区大小
)

var (
	mode     = flag.String("mode", "send", "运行模式: send (连接并发送) 或 receive (监听并接收)") // 恢复模式说明
	file     = flag.String("file", "", "要发送的文件路径 (send 模式)")                       // 发送端仍需指定文件
	dir      = flag.String("dir", ".", "保存文件的目录路径 (receive 模式)")                   // 接收端指定目录
	addr     = flag.String("addr", "localhost:8080", "网络地址 (连接地址 for send, 监听地址 for receive)")
	badFile  = "failed_files.log" // 记录传输失败的文件
	noSplice = flag.Bool("no-splice", false, "接收端不使用 splice 系统调用 (使用标准 Go io.Copy)")
	sndBuf   = flag.Int("sndbuf", 0, "设置 TCP 发送缓冲区大小 (4194304字节, 0=系统默认)")
	rcvBuf   = flag.Int("rcvbuf", 0, "设置 TCP 接收缓冲区大小 (4194304字节, 0=系统默认)")
	oDirect  = flag.Bool("odirect", false, "接收端打开目标文件时使用 O_DIRECT (Linux only, 绕过页缓存, 谨慎使用!)")            // 添加缺失的 O_DIRECT 标志定义
	sizeStr  = flag.String("size", "", "要传输的数据大小 (用于 send -file /dev/zero 时指定大小, e.g., 1G, 500M, 1024K)") // 更新 size 说明
	prewarm  = flag.Bool("prewarm", false, "发送端在程序启动时预热文件到页缓存 (仅 send 模式)")
)

type FileInfoError struct {
	FilePath string
	Err      error
}

func (e *FileInfoError) Error() string {
	return fmt.Sprintf("文件 '%s' 处理失败: %v", e.FilePath, e.Err)
}

func (e *FileInfoError) Unwrap() error {
	return e.Err
}

type SendfileIOError struct {
	FilePath string
	Offset   int64
	Err      error
}

func (e *SendfileIOError) Error() string {
	return fmt.Sprintf("文件 '%s' 在偏移量 %d 处 sendfile I/O 错误: %v", e.FilePath, e.Offset, e.Err)
}

func (e *SendfileIOError) Unwrap() error {
	return e.Err
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "使用方法: ftgo [参数]\n")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\n示例:")
		fmt.Fprintln(os.Stderr, "  发送本地文件:")
		fmt.Fprintln(os.Stderr, "    ftgo -mode receive -dir ./received_files -addr localhost:8080 -no-splice")
		fmt.Fprintln(os.Stderr, "    ftgo -mode send -file testfile.dat -addr localhost:8080 -prewarm")
		fmt.Fprintln(os.Stderr, "  传输性能测试示例:")
		fmt.Fprintln(os.Stderr, "    发送端: ftgo -mode send -file /dev/zero -size 10G -addr localhost:8080 -prewarm")
		fmt.Fprintln(os.Stderr, "    接收端: ftgo -mode receive -dir ./received_files -addr localhost:8080 -no-splice")
		fmt.Fprintln(os.Stderr, "\n注意事项:")
		fmt.Fprintln(os.Stderr, "  - 由于使用了 splice、sendfile 和 fallocate 等系统调用，建议在 Linux 系统上运行此程序。")
		fmt.Fprintln(os.Stderr, "  - 谨慎使用 -odirect 标志，因为它会绕过页缓存，可能影响性能，并且有严格的对齐要求。")
		fmt.Fprintln(os.Stderr, "  - 发送 /dev/zero 时必须指定 -size 参数。")
	}
	flag.Parse()

	// 强制要求Linux系统
	if runtime.GOOS != "linux" {
		log.Fatal("错误: 此程序只能在Linux系统上运行")
	}

	// 如果没有提供任何参数，则显示用法并退出
	if len(os.Args) == 1 {
		flag.Usage()
		os.Exit(0)
	}

	// 参数校验
	if *mode == "send" {
		if *file == "" {
			log.Fatal("错误: send 模式下必须指定 -file 参数")
		}
		// 如果发送 /dev/zero，则必须指定 -size
		if *file == "/dev/zero" && *sizeStr == "" {
			log.Fatal("错误: 使用 -file /dev/zero 时必须指定 -size 参数")
		}
		if *file == "/dev/zero" {
			if _, err := parseSize(*sizeStr); err != nil {
				log.Fatalf("错误: 使用 -file /dev/zero 时 -size 参数无效: %v", err)
			}
		}
	}
	if *mode == "receive" && *dir == "" {
		log.Fatal("错误: receive 模式下必须指定 -dir 参数")
	}

	// 保留对非Linux系统的额外警告
	if runtime.GOOS != "linux" {
		log.Printf("\x1b[33m警告: 当前系统 %s 非 Linux，程序功能可能受限\x1b[0m", runtime.GOOS)
	}

	// --- 文件预热 (如果需要) ---
	if *mode == "send" && *prewarm && *file != "/dev/zero" {
		// 直接调用 doPrewarm，它将自行获取文件大小并预热整个文件
		if err := doPrewarm(*file); err != nil {
			// 预热失败仅记录警告，不中断程序
			log.Printf("\x1b[33m警告: 文件预热失败: %v\x1b[0m", err)
		}
	}

	switch *mode {
	case "send": // 客户端
		var err error
		if *file == "/dev/zero" {
			// 对于 /dev/zero，调用特殊版本的 sender 或传递大小
			// 这里我们先简单处理，后面修改 sender 函数内部逻辑
			log.Printf("检测到发送 /dev/zero，将使用 -size 指定的大小并采用标准网络写入")
			err = sender(*file, *addr) // sender 内部需要处理 /dev/zero 情况
		} else {
			err = sender(*file, *addr)
		}
		if err != nil {
			if _, ok := err.(*net.OpError); ok {
				log.Fatalf("\x1b[31m发送端网络错误: %v\x1b[0m", err)
			} else if _, ok := err.(*FileInfoError); ok {
				log.Printf("\x1b[31m发送端文件错误: %v\x1b[0m", err)
				os.Exit(1)
			} else if e, ok := err.(*SendfileIOError); ok {
				log.Printf("\x1b[31m发送端传输错误: %v\x1b[0m", e)
				logFailedFile(*file, e.Error())
				os.Exit(1)
			} else {
				log.Printf("\x1b[31m发送端未知错误: %v\x1b[0m", err)
				os.Exit(1)
			}
		} else {
			fmt.Println("文件发送成功完成.")
		}
	case "receive": // 服务器
		err := receiver(*dir, *addr, *noSplice) // receiver 内部需要处理 /dev/null 情况
		if err != nil {
			log.Fatalf("\x1b[31m接收端错误: %v\x1b[0m", err)
		}

	default:
		log.Fatalf("错误: 无效的模式 %q. 请使用 'send' 或 'receive'", *mode) // 恢复默认错误消息
	}
}

// 记录失败的文件
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
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	fmt.Printf("\r\033[K进度: 0.00%% (0/%d bytes), 速度: 0.00 MB/s", totalSize)
	for {
		select {
		case <-ticker.C:
			currentTransferred := atomic.LoadInt64(transferred)
			elapsed := time.Since(startTime).Seconds()
			if elapsed < 0.1 {
				elapsed = 0.1
			}
			speed := float64(currentTransferred) / elapsed / 1024 / 1024
			var progress float64
			if totalSize > 0 {
				progress = float64(currentTransferred) * 100 / float64(totalSize)
			} else if currentTransferred == 0 && totalSize == 0 {
				progress = 100.0
			} else {
				progress = 0.0
			}
			if progress > 100.0 {
				progress = 100.0
			}
			fmt.Printf("\r\033[K进度: %.2f%% (%s/%s bytes), 速度: %.2f MB/s", progress, formatWithCommas(currentTransferred), formatWithCommas(totalSize), speed)
		case <-done:
			currentTransferred := atomic.LoadInt64(transferred)
			elapsed := time.Since(startTime).Seconds()
			if elapsed < 0.1 {
				elapsed = 0.1
			}
			speed := float64(currentTransferred) / elapsed / 1024 / 1024
			var progress float64
			if totalSize > 0 {
				progress = float64(currentTransferred) * 100 / float64(totalSize)
			} else {
				progress = 100.0
			}
			if progress > 100.0 {
				progress = 100.0
			}
			fmt.Printf("\r\033[K进度: %.2f%% (%s/%s bytes), 速度: %.2f MB/s\n", progress, formatWithCommas(currentTransferred), formatWithCommas(totalSize), speed)
			return
		}
	}
}

func sender(filePath string, connectAddr string) error {
	isDevZero := (filePath == "/dev/zero")
	conn, err := net.DialTimeout("tcp", connectAddr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("连接失败 %s: %w", connectAddr, err)
	}
	defer conn.Close()
	log.Printf("\x1b[32m已连接到接收端 %s\x1b[0m", connectAddr)

	// 尝试设置 TCP 发送缓冲区
	if tcpConn, ok := conn.(*net.TCPConn); ok && *sndBuf > 0 {
		if err := tcpConn.SetWriteBuffer(*sndBuf); err != nil {
			log.Printf("\x1b[33m警告: 设置 TCP 发送缓冲区为 %d 失败: %v\x1b[0m", *sndBuf, err)
		} else {
			// 无法直接通过 Go API 获取实际大小，需要 OS 工具检查
			log.Printf("已尝试设置 TCP 发送缓冲区为 %d (实际大小需通过 OS 工具检查)", *sndBuf)
		}
	}

	var fileSize int64
	var fileName string
	// var err error // err 已在 net.DialTimeout 处通过 := 声明，移除重复声明

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
			return &FileInfoError{FilePath: filePath, Err: err}
		}
		fileSize = fileInfo.Size()
		fileName = fileInfo.Name() // 获取真实文件名
	}
	fileNameBytes := []byte(fileName)
	fileNameLen := uint16(len(fileNameBytes))

	// 1. 发送文件名长度 (2 bytes)
	lenBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBytes, fileNameLen)
	if _, err := conn.Write(lenBytes); err != nil {
		return fmt.Errorf("发送文件名长度失败: %w", err)
	}
	log.Printf("\x1b[32m已发送文件名长度: %d\x1b[0m", fileNameLen)

	// 2. 发送文件名
	if _, err := conn.Write(fileNameBytes); err != nil {
		return fmt.Errorf("发送文件名失败: %w", err)
	}
	log.Printf("\x1b[32m已发送文件名: %s\x1b[0m", fileName)

	// 3. 发送文件大小 (8 bytes)
	sizeBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(sizeBytes, uint64(fileSize))
	if _, err := conn.Write(sizeBytes); err != nil {
		return fmt.Errorf("发送文件大小失败: %w", err)
	}
	log.Printf("\x1b[32m已发送文件大小: %s\x1b[0m", formatWithCommas(fileSize))
	// 预热逻辑已移到 main 函数
	// (移除因之前 diff 错误而残留的多余右括号)

	// 对于 /dev/zero，我们不需要打开它然后用 sendfile，直接写网络
	// 对于常规文件，才需要打开并获取 fd
	// --- 准备传输 ---
	var srcFile *os.File // 用于标准写入或获取 fd
	var srcFd int = -1   // 用于 sendfile
	if !isDevZero {
		srcFile, err = os.Open(filePath)
		if err != nil {
			return &FileInfoError{FilePath: filePath, Err: fmt.Errorf("打开源文件失败: %w", err)}
		}
		defer srcFile.Close()
		srcFd = int(srcFile.Fd())
	}

	// 获取网络连接的 fd (sendfile 需要) 或直接使用 conn (标准写入需要)
	var dstFd int = -1 // 初始化为无效值
	if !isDevZero {    // 移除 runtime.GOOS 检查，因为程序入口已保证是 Linux
		tcpConn, ok := conn.(*net.TCPConn)
		if !ok {
			log.Printf("\x1b[33m警告: 连接不是 TCP 连接，无法使用 sendfile，将回退到标准写入\x1b[0m")
			// isDevZero 已经是 false, 所以会进入下面的 else 分支
		} else {
			dstFile, err := tcpConn.File()
			if err != nil {
				log.Printf("\x1b[33m警告: 获取连接文件描述符失败 (%v)，无法使用 sendfile，将回退到标准写入\x1b[0m", err)
				// isDevZero 已经是 false, 所以会进入下面的 else 分支
			} else {
				defer dstFile.Close()
				dstFd = int(dstFile.Fd())
			}
		}
	}

	var transferred int64
	startTime := time.Now()
	done := make(chan struct{})
	go displayProgress(fileSize, &transferred, startTime, done)
	defer close(done)

	// var offset int64 = 0 // 移到 sendfile 逻辑块内部
	totalSent := int64(0)

	if fileSize == 0 {
		atomic.StoreInt64(&transferred, 0)
		time.Sleep(100 * time.Millisecond)
		return nil
	}

	// 根据情况选择传输方式
	if !isDevZero && srcFd != -1 && dstFd != -1 { // 移除 runtime.GOOS 检查
		// 使用 sendfile (Linux 上的常规文件)
		log.Printf("使用 sendfile 传输文件 %s", filePath)
		var offset int64 = 0 // sendfile 需要 offset，在此声明
		for totalSent < fileSize {
			remaining := fileSize - totalSent
			count := int64(copyBufferSize)
			if remaining < count {
				count = remaining
			}
			currentOffset := offset // 记录当前偏移量用于错误报告
			n, err := unix.Sendfile(dstFd, srcFd, &offset, int(count))
			if err != nil {
				if errno, ok := err.(unix.Errno); ok && errno == unix.EIO {
					return &SendfileIOError{FilePath: filePath, Offset: currentOffset, Err: err}
				}
				if errno, ok := err.(unix.Errno); ok && (errno == unix.EPIPE || errno == unix.ECONNRESET) {
					log.Printf("发送端检测到连接断开 (sendfile): %v", err)
					return fmt.Errorf("连接已断开: %w", err)
				}
				return fmt.Errorf("sendfile 在偏移量 %d 失败: %w", currentOffset, err)
			}
			if n == 0 {
				if totalSent < fileSize {
					return fmt.Errorf("sendfile 返回 0 但文件未传输完成 (已发送 %d / %d)", totalSent, fileSize)
				}
				break // 正常完成
			}
			sentBytes := int64(n)
			atomic.AddInt64(&transferred, sentBytes)
			totalSent += sentBytes
		}
		log.Printf("\x1b[32mSendfile 完成，总共发送 %d bytes\x1b[0m", totalSent)
	} else {
		// 使用标准网络写入 (发送 /dev/zero 或 非 Linux 或 获取 fd 失败)
		if isDevZero {
			log.Printf("使用标准网络写入传输 /dev/zero 数据")
		} else {
			log.Printf("使用标准网络写入传输文件 %s (sendfile 不可用)", filePath) // 移除 "非 Linux"
		}
		buffer := make([]byte, copyBufferSize)
		var reader io.Reader
		if isDevZero {
			reader = &zeroReader{size: fileSize} // 使用自定义的 reader 模拟
		} else {
			if srcFile == nil {
				return fmt.Errorf("无法获取源文件句柄进行标准读取")
			}
			if _, err := srcFile.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("无法重置文件指针: %w", err)
			}
			reader = srcFile
		}

		progressWriter := &progressUpdater{conn: conn, transferred: &transferred}
		written, err := io.CopyBuffer(progressWriter, reader, buffer)
		totalSent = written
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && (opErr.Err == unix.EPIPE || opErr.Err == unix.ECONNRESET) {
				log.Printf("发送端检测到连接断开 (标准写入): %v", err)
				return fmt.Errorf("连接已断开: %w", err)
			}
			return fmt.Errorf("标准写入失败 (已发送 %d bytes): %w", totalSent, err)
		}
		log.Printf("\x1b[32m标准网络写入完成，总共发送 %d bytes\x1b[0m", totalSent)
	} // End of if/else for transfer method

	// Short sleep to allow progress display to potentially catch up
	time.Sleep(100 * time.Millisecond)

	// Final check: ensure total sent bytes match the expected file size
	if totalSent != fileSize {
		return fmt.Errorf("最终发送字节数 (%d) 与预期文件大小 (%d) 不符", totalSent, fileSize)
	}

	log.Printf("发送完成，总共发送 %s bytes", formatWithCommas(totalSent)) // Generic completion message
	return nil
} // End of sender function

// --- 文件预热函数 (使用 Readahead 预热整个文件) ---
func doPrewarm(filePath string) error { // 移除 prewarmBytes 参数
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
	// 使用 unix.Syscall 调用 readahead(2)
	// int fd, off64_t offset, size_t count
	// 使用获取到的文件大小进行 Readahead
	_, _, err = unix.Syscall(unix.SYS_READAHEAD, uintptr(fd), uintptr(0), uintptr(fileSize)) // 使用 = 赋值给已声明的 err

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

func receiver(dirPath string, listenAddr string, useStandardCopy bool) error {
	// 添加变量来跟踪所有文件的传输统计
	var totalBytesReceived int64
	var totalFilesReceived int
	var startTime = time.Now()

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", listenAddr, err) // 监听失败是致命错误
	}
	defer listener.Close()
	log.Printf("\x1b[32m服务器启动，正在监听 %s\x1b[0m", listenAddr)

	for { // 无限循环，顺序处理连接
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("\x1b[33m警告: 接受连接失败: %v\x1b[0m", err)
			// 检查是否是监听器关闭导致的错误
			if opErr, ok := err.(*net.OpError); ok && opErr.Err.Error() == "use of closed network connection" {
				log.Println("监听器已关闭，服务器退出。")
				return nil // 正常退出
			}
			continue // 其他接受错误，继续等待下一个连接
		}

		// --- 开始处理单个连接 ---
		remoteAddrStr := conn.RemoteAddr().String() // 获取远程地址字符串，方便日志记录
		log.Printf("[%s] 接收到连接，开始处理...", remoteAddrStr)

		// 声明变量来保存传输结果，以便在匿名函数外访问
		var fileTransferred int64
		var fileReceiveError error
		var fileTransferName string
		var connectionStart time.Time

		// 尝试设置 TCP 接收缓冲区
		if tcpConn, ok := conn.(*net.TCPConn); ok && *rcvBuf > 0 {
			if err := tcpConn.SetReadBuffer(*rcvBuf); err != nil {
				log.Printf("\x1b[33m[%s] 警告: 设置 TCP 接收缓冲区为 %d 失败: %v\x1b[0m", remoteAddrStr, *rcvBuf, err)
			} else {
				// 无法直接通过 Go API 获取实际大小，需要 OS 工具检查
				log.Printf("[%s] 已尝试设置 TCP 接收缓冲区为 %d (实际大小需通过 OS 工具检查)", remoteAddrStr, *rcvBuf)
			}
		}

		// 在循环内处理单个连接
		func(conn net.Conn) {
			defer conn.Close()

			// 记录当前连接开始时间
			connectionStart = time.Now()

			// 声明变量
			var targetPath string
			var receiveErr error
			var fileName string
			var fileSize int64
			var totalReceived int64 = 0

			// 错误处理和清理
			defer func() {
				if receiveErr != nil {
					log.Printf("\x1b[31m[%s] 错误: %v\x1b[0m", remoteAddrStr, receiveErr)
				}
				// 保存结果以便外部访问
				fileReceiveError = receiveErr
				fileTransferred = totalReceived
				fileTransferName = fileName
				log.Printf("[%s] 连接已关闭", remoteAddrStr)
			}()

			// 1. 读取文件名长度 (2 bytes)
			lenBytes := make([]byte, 2)
			if _, err := io.ReadFull(conn, lenBytes); err != nil {
				receiveErr = fmt.Errorf("读取文件名长度失败: %w", err)
				return // 退出匿名函数
			}
			fileNameLen := binary.BigEndian.Uint16(lenBytes)
			log.Printf("\x1b[32m[%s] 接收到文件名长度: %d\x1b[0m", remoteAddrStr, fileNameLen)

			// 2. 读取文件名
			fileNameBytes := make([]byte, fileNameLen)
			if _, err := io.ReadFull(conn, fileNameBytes); err != nil {
				receiveErr = fmt.Errorf("读取文件名失败: %w", err)
				return // 退出匿名函数
			}
			fileName = string(fileNameBytes)
			log.Printf("\x1b[32m[%s] 接收到文件名: %s\x1b[0m", remoteAddrStr, fileName)

			// 3. 读取文件大小信息 (8 bytes)
			sizeBytes := make([]byte, 8)
			if _, err := io.ReadFull(conn, sizeBytes); err != nil {
				receiveErr = fmt.Errorf("读取文件大小失败: %w", err)
				return // 退出匿名函数
			}
			fileSize = int64(binary.BigEndian.Uint64(sizeBytes))
			log.Printf("\x1b[32m[%s] 文件大小: %s 字节\x1b[0m", remoteAddrStr, formatWithCommas(fileSize))

			// 检查目标是否为 /dev/null，并设置 targetPath
			isDevNull := (dirPath == "/dev/null")
			if isDevNull {
				targetPath = "/dev/null"
				log.Printf("\x1b[32m[%s] 接收到文件名 '%s'，将数据写入 /dev/null\x1b[0m", remoteAddrStr, fileName)
			} else {
				// 仅在目标不是 /dev/null 时才创建目录
				if err := os.MkdirAll(dirPath, 0755); err != nil {
					receiveErr = fmt.Errorf("创建目录 '%s' 失败: %w", dirPath, err)
					return // 退出匿名函数
				}
				targetPath = filepath.Join(dirPath, fileName)
				log.Printf("\x1b[32m[%s] 将文件 '%s' 保存到: %s\x1b[0m", remoteAddrStr, fileName, targetPath)
			}
			// targetPath 现在已根据 isDevNull 正确设置

			// 创建或打开目标文件/设备
			// var openFlags int // 移到 else 块内部声明
			// 创建或打开目标文件/设备
			var dstFile *os.File
			var err error
			if isDevNull {
				// 直接打开 /dev/null，忽略 O_DIRECT
				dstFile, err = os.OpenFile("/dev/null", os.O_WRONLY, 0)
				if err != nil {
					receiveErr = fmt.Errorf("打开 /dev/null 失败: %w", err)
					return
				}
			} else {
				// 打开常规文件，处理 O_DIRECT
				openFlags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
				if *oDirect {
					// if runtime.GOOS == "linux" { // 移除检查，因为入口已保证是 Linux
					openFlags |= unix.O_DIRECT
					log.Printf("\x1b[33m[%s] 警告: 使用 O_DIRECT 打开文件 %s。这将绕过页缓存，可能影响性能，并有严格的对齐要求。标准 IO 模式 (-no-splice) 可能无法正常工作。\x1b[0m", remoteAddrStr, targetPath)
					// } else { // 移除 else 分支，因为非 Linux 情况已在入口处理
					//	log.Printf("[%s] 警告: -odirect 标志仅在 Linux 上生效，当前系统 %s 将忽略此标志。", remoteAddrStr, runtime.GOOS)
					// }
				}
				dstFile, err = os.OpenFile(targetPath, openFlags, 0644)
				if err != nil {
					receiveErr = &FileInfoError{FilePath: targetPath, Err: fmt.Errorf("创建/打开目标文件 '%s' 失败: %w", targetPath, err)}
					return
				}
			}
			defer dstFile.Close()

			// 预分配（仅对常规文件且在 Linux 上）
			if !isDevNull && fileSize > 0 { // 移除 runtime.GOOS 检查
				// 检查是否真的打开了常规文件（dstFile 可能因错误为 nil）
				if dstFile != nil { // 确保 dstFile 不是 nil
					if err := unix.Fallocate(int(dstFile.Fd()), 0, 0, fileSize); err != nil {
						// 预分配失败通常不是致命错误，记录警告即可
						log.Printf("\x1b[33m[%s] 警告: 预分配文件空间 '%s' 失败: %v\x1b[0m", remoteAddrStr, targetPath, err)
					}
				}
			}

			// 设置进度显示
			var transferred int64
			startTime := time.Now()
			done := make(chan struct{})
			defer close(done)
			go displayProgress(fileSize, &transferred, startTime, done)

			if fileSize == 0 {
				atomic.StoreInt64(&transferred, 0)
				time.Sleep(100 * time.Millisecond) // 确保进度显示能更新为 100%
				return                             // 空文件接收成功，退出匿名函数
			}

			// 获取TCP连接的文件描述符
			tcpConn, ok := conn.(*net.TCPConn)
			if !ok {
				receiveErr = fmt.Errorf("连接不是 TCP 连接")
				return // 退出匿名函数
			}

			srcFile, err := tcpConn.File()
			if err != nil {
				receiveErr = fmt.Errorf("获取连接文件描述符失败: %w", err)
				return // 退出匿名函数
			}
			defer srcFile.Close()

			// 获取文件描述符
			srcFd := int(srcFile.Fd())
			dstFd := int(dstFile.Fd())

			// --- 开始传输 ---
			if useStandardCopy {
				log.Printf("[%s] 使用标准 IO 复制而不是 splice", remoteAddrStr)
				buffer := make([]byte, copyBufferSize)

				// 提高性能的并发读写 (修复竞态条件)
				readCh := make(chan []byte) // 通道传递数据副本
				errorCh := make(chan error, 1)
				doneCh := make(chan struct{})

				// 启动读取goroutine
				go func() {
					defer close(readCh)
					defer close(doneCh)
					for totalReceived < fileSize {
						n, err := conn.Read(buffer)
						if err != nil {
							if n == 0 && (err == io.EOF || err == io.ErrUnexpectedEOF) {
								return
							}
							select {
							case errorCh <- fmt.Errorf("[%s] 读取数据失败: %w", remoteAddrStr, err):
							default:
							}
							return
						}
						if n == 0 {
							break
						}
						// 创建数据副本并发送
						dataCopy := make([]byte, n)
						copy(dataCopy, buffer[:n])
						readCh <- dataCopy
					}
				}()

				// 处理写入
				for data := range readCh { // 接收数据副本
					written, err := dstFile.Write(data) // 写入副本
					if err != nil {
						select {
						case errorCh <- fmt.Errorf("[%s] 写入文件 '%s' 失败: %w", remoteAddrStr, targetPath, err):
						default:
						}
						break
					}
					// 检查写入的字节数是否与接收到的数据块大小一致
					if written != len(data) {
						select {
						case errorCh <- fmt.Errorf("[%s] 写入文件 '%s' 不完整: 预期 %d, 实际 %d", remoteAddrStr, targetPath, len(data), written):
						default:
						}
						break
					}

					// 更新进度
					atomic.AddInt64(&transferred, int64(written))
					totalReceived += int64(written) // totalReceived 仍然累加写入的字节数
				}

				// 等待读取完成或出错
				select {
				case <-doneCh:
				// 读取完成
				case err := <-errorCh:
					if receiveErr == nil { // 只记录第一个错误
						receiveErr = err
					}
				}

			} else { // 使用 splice
				// 使用splice系统调用
				log.Printf("[%s] 使用 splice 系统调用传输数据", remoteAddrStr)
				pipeFds := make([]int, 2)
				if err := unix.Pipe(pipeFds); err != nil {
					receiveErr = fmt.Errorf("创建管道失败: %w", err)
					return // 退出匿名函数
				}
				defer unix.Close(pipeFds[0])
				defer unix.Close(pipeFds[1])

				// 设置管道缓冲区大小为最大值（可选）
				unix.FcntlInt(uintptr(pipeFds[0]), unix.F_SETPIPE_SZ, copyBufferSize*4)
				unix.FcntlInt(uintptr(pipeFds[1]), unix.F_SETPIPE_SZ, copyBufferSize*4)

				for totalReceived < fileSize {
					// 从socket读取数据到管道
					n, err := unix.Splice(srcFd, nil, pipeFds[1], nil, copyBufferSize, unix.SPLICE_F_MOVE|unix.SPLICE_F_MORE)
					if err != nil {
						receiveErr = fmt.Errorf("从 socket 到管道的 splice 操作失败: %w", err)
						break // 退出循环去处理错误
					}
					if n == 0 {
						break // 连接关闭
					}

					// 从管道写入数据到文件
					written, err := unix.Splice(pipeFds[0], nil, dstFd, nil, int(n), unix.SPLICE_F_MOVE|unix.SPLICE_F_MORE)
					if err != nil {
						receiveErr = fmt.Errorf("从管道到文件 '%s' 的 splice 操作失败: %w", targetPath, err)
						break // 退出循环去处理错误
					}

					if written != n {
						receiveErr = fmt.Errorf("splice 写入文件 '%s' 不完整: 预期 %d, 实际 %d", targetPath, n, written)
						break // 退出循环去处理错误
					}

					atomic.AddInt64(&transferred, written)
					totalReceived += written
				}

				if receiveErr == nil { // 只有在 splice 循环中没出错才检查大小
					if totalReceived != fileSize {
						receiveErr = fmt.Errorf("文件 '%s' 接收到的数据大小 (%d) 与预期大小 (%d) 不符", fileName, totalReceived, fileSize)
					}
				}
			} // End of if/else for useStandardCopy
			// --- 传输结束 ---
		}(conn) // 结束并调用匿名函数

		// 显示传输完成信息
		if fileTransferred > 0 && fileReceiveError == nil {
			// 计算文件传输速度
			elapsed := time.Since(connectionStart).Seconds()
			fileAvgSpeed := float64(fileTransferred) / elapsed / 1024 / 1024
			log.Printf("\x1b[32m[%s] 传输完成，文件 '%s' 接收了 %s bytes，速度: %.2f MB/s\x1b[0m",
				remoteAddrStr, fileTransferName, formatWithCommas(fileTransferred), fileAvgSpeed)

			// 更新总统计
			atomic.AddInt64(&totalBytesReceived, fileTransferred)
			totalFilesReceived++
		}

		// 计算总体平均速度并显示
		totalElapsed := time.Since(startTime).Seconds()
		if totalBytesReceived > 0 && totalElapsed > 0 {
			overallAvgSpeed := float64(totalBytesReceived) / totalElapsed / 1024 / 1024
			log.Printf("\x1b[32m累计接收: %d 个文件，总大小 %s bytes，平均速度: %.2f MB/s\x1b[0m",
				totalFilesReceived, formatWithCommas(totalBytesReceived), overallAvgSpeed)
		}

		log.Printf("等待下一个连接...")
	} // End of for loop
	// 通常不会执行到这里 (因为 for 循环是无限的)
	return nil
}

// 移除 networkSender 和 networkReceiver 函数

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

type zeroReader struct {
	size    int64
	readPos int64
}

func (zr *zeroReader) Read(p []byte) (n int, err error) {
	if zr.readPos >= zr.size {
		return 0, io.EOF // 已达到指定大小
	}
	remaining := zr.size - zr.readPos
	readLen := int64(len(p))
	if remaining < readLen {
		readLen = remaining
	}
	// 不需要填充 p，因为我们只关心读取的字节数 n
	// for i := range p[:readLen] { p[i] = 0 } // 如果需要严格模拟零字节
	zr.readPos += readLen
	return int(readLen), nil
}

type progressUpdater struct {
	conn        net.Conn
	transferred *int64
}

func (pu *progressUpdater) Write(p []byte) (n int, err error) {
	n, err = pu.conn.Write(p)
	if n > 0 {
		atomic.AddInt64(pu.transferred, int64(n))
	}
	return n, err
}
