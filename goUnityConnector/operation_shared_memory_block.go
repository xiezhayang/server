//go:build windows
// +build windows

package goUnityConnector

import (
	"fmt"
	"log"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type OperationMemoryBlock struct {
	config   OperationMemoryBlockConfig
	hMapFile syscall.Handle
	shmPtr   uintptr
	header   *OperationHeader
	commands []OperationCommand // 指向共享内存中的环形缓冲区（仅引用）
	isOwner  bool               // 是否为创建者（Unity端通常为true）

	// 统计信息（本地内存，非共享）
	stats struct {
		totalReads  uint64 // Go端读取次数
		totalWrites uint64 // Unity端写入次数（通过监控header.Timestamp变化统计）
		overflow    uint64 // 缓冲区溢出次数
	}
}

// NewOperationMemoryBlock 创建或打开操作内存块
// create=true: 创建新共享内存（Unity端调用）
// create=false: 打开现有共享内存（Go端调用）
func NewOperationMemoryBlock(config OperationMemoryBlockConfig, create bool) (*OperationMemoryBlock, error) {
	if config.Capacity == 0 {
		return nil, fmt.Errorf("Capacity必须>0")
	}

	// 计算总大小：头部 + 环形缓冲区
	headerSize := unsafe.Sizeof(OperationHeader{})
	commandSize := unsafe.Sizeof(OperationCommand{})
	totalSize := headerSize + uintptr(config.Capacity)*commandSize

	omb := &OperationMemoryBlock{
		config:  config,
		isOwner: create,
	}

	// 转换名称为UTF-16
	namePtr, err := syscall.UTF16PtrFromString(config.Name)
	if err != nil {
		return nil, fmt.Errorf("名称转换失败: %v", err)
	}

	if create {
		// 创建共享内存（Unity端）
		hMap, _, err := procCreateFileMappingW.Call(
			uintptr(INVALID_HANDLE_VALUE), // 使用系统页面文件
			0,                             // 安全属性
			uintptr(PAGE_READWRITE),       // 保护模式
			0,                             // 高位大小
			uintptr(totalSize),            // 低位大小
			uintptr(unsafe.Pointer(namePtr)),
		)

		if hMap == 0 {
			return nil, fmt.Errorf("创建共享内存失败: %v", err)
		}
		omb.hMapFile = syscall.Handle(hMap)
	} else {
		// 打开现有共享内存（Go端）
		hMap, _, err := procOpenFileMappingW.Call(
			uintptr(FILE_MAP_ALL_ACCESS), // ← 访问权限
			uintptr(0),                   // ← bInheritHandle = FALSE
			uintptr(unsafe.Pointer(namePtr)),
		)

		if hMap == 0 {
			return nil, fmt.Errorf("打开共享内存失败: %v", err)
		}
		omb.hMapFile = syscall.Handle(hMap)
	}

	// 映射共享内存视图
	ptr, _, err := procMapViewOfFile.Call(
		uintptr(omb.hMapFile),
		uintptr(FILE_MAP_ALL_ACCESS),
		0,
		0,
		uintptr(totalSize),
	)

	if ptr == 0 {
		procCloseHandle.Call(uintptr(omb.hMapFile))
		return nil, fmt.Errorf("映射共享内存失败: %v", err)
	}

	omb.shmPtr = ptr

	// 设置指针
	omb.header = (*OperationHeader)(unsafe.Pointer(omb.shmPtr))
	commandsStart := omb.shmPtr + headerSize

	// 创建切片指向环形缓冲区（不持有数据，仅引用共享内存）
	sliceHeader := &struct {
		data uintptr
		len  int
		cap  int
	}{
		data: commandsStart,
		len:  int(config.Capacity),
		cap:  int(config.Capacity),
	}
	omb.commands = *(*[]OperationCommand)(unsafe.Pointer(sliceHeader))

	// 如果是创建者（Unity端），初始化头部
	if create {
		omb.Initialize()
	}

	action := "打开"
	if create {
		action = "创建"
	}
	log.Printf("操作内存块 %s 已%s (容量: %d 命令, 大小: %d 字节)",
		config.Name, action, config.Capacity, totalSize)

	return omb, nil
}

// Initialize 初始化操作内存块头部（仅由Unity端调用）
func (omb *OperationMemoryBlock) Initialize() {
	// 使用原子操作确保跨进程可见性
	atomic.StoreUint32(&omb.header.Version, 1)
	atomic.StoreUint32(&omb.header.Capacity, omb.config.Capacity)
	atomic.StoreUint32(&omb.header.ReadIndex, 0)  // Go读取位置
	atomic.StoreUint32(&omb.header.WriteIndex, 0) // Unity写入位置
	atomic.StoreUint32(&omb.header.Count, 0)
	atomic.StoreUint32(&omb.header.Flags, 0)
	atomic.StoreUint64(&omb.header.Timestamp, uint64(time.Now().UnixNano()))

	if omb.config.EnableLog {
		log.Printf("操作内存块 %s 已初始化", omb.config.Name)
	}
}

// ReadCommands 读取命令（支持批量读取和单个读取）
// maxCount <= 0: 读取所有可用命令
// maxCount = 1: 读取单个命令（替代原ReadSingleCommand）
// 返回实际读取的命令列表，可能为空
func (omb *OperationMemoryBlock) ReadCommands(maxCount int) []OperationCommand {
	// 原子获取当前可读命令数
	currentCount := atomic.LoadUint32(&omb.header.Count)
	if currentCount == 0 {
		return nil
	}

	// 确定要读取的数量
	toRead := int(currentCount)
	if maxCount > 0 && maxCount < toRead {
		toRead = maxCount
	}

	// 原子获取读指针位置
	currentRead := atomic.LoadUint32(&omb.header.ReadIndex)
	capacity := omb.config.Capacity

	results := make([]OperationCommand, 0, toRead)
	var lastSeq uint32 = 0
	seqInitialized := false
	readCount := 0

	// 逐个读取命令并进行序列号验证
	for i := 0; i < toRead; i++ {
		index := (currentRead + uint32(i)) % capacity
		cmd := omb.commands[index]

		// 序列号连续性验证
		if seqInitialized {
			expectedSeq := lastSeq + 1

			// 处理32位环绕：0xFFFFFFFF → 0 为有效环绕
			if lastSeq == 0xFFFFFFFF && cmd.Seq == 0 {
				// 有效环绕，继续读取
			} else if cmd.Seq != expectedSeq {
				// 序列号不连续，可能发生数据撕裂或乱序
				if omb.config.EnableLog {
					log.Printf("警告: 序列号不连续 at index %d (前序=%d, 期望=%d, 实际=%d)",
						index, lastSeq, expectedSeq, cmd.Seq)
				}
				break // 停止读取后续命令
			}
		} else if cmd.Seq == 0 && omb.config.EnableLog {
			// 第一个命令且序列号为0（可能未初始化）
			log.Printf("警告: 读取到Seq=0的命令")
		}

		results = append(results, cmd)
		lastSeq = cmd.Seq
		readCount++
		seqInitialized = true
	}

	if readCount == 0 {
		return nil
	}

	// 原子更新读指针
	newRead := (currentRead + uint32(readCount)) % capacity
	atomic.StoreUint32(&omb.header.ReadIndex, newRead)

	// 原子减少计数（正确方式）
	atomic.AddUint32(&omb.header.Count, -uint32(readCount))

	// 更新统计
	atomic.AddUint64(&omb.stats.totalReads, uint64(readCount))

	// 日志输出
	if omb.config.EnableLog && readCount > 0 {
		log.Printf("读取命令: 数量=%d, 首命令类型=%v, 首命令Seq=%d",
			readCount, results[0].Type, results[0].Seq)
	}

	return results
}

// WriteCommand 写入单个命令（Unity端调用，Go端不应使用）
// 增强版：包含内存屏障和序列号验证，避免修改调用方数据
func (omb *OperationMemoryBlock) WriteCommand(cmd *OperationCommand) bool {
	// Go端调用警告（设计为Unity→Go单向通信）
	if !omb.isOwner && omb.config.EnableLog {
		log.Printf("警告: Go端不应调用WriteCommand，OperationMemoryBlock设计为Unity→Go单向通信")
	}

	currentWrite := atomic.LoadUint32(&omb.header.WriteIndex)
	currentCount := atomic.LoadUint32(&omb.header.Count)

	// 检查缓冲区是否已满
	if currentCount >= omb.config.Capacity {
		atomic.AddUint64(&omb.stats.overflow, 1)
		if omb.config.EnableLog {
			log.Printf("操作缓冲区溢出，丢弃命令: %v", cmd.Type)
		}
		return false
	}

	// 创建命令副本，避免修改调用方数据
	cmdCopy := *cmd

	// 设置时间戳（纳秒精度）
	cmdCopy.Timestamp = uint64(time.Now().UnixNano())

	// 生成序列号（使用header.Flags作为全局序列号计数器）
	seq := atomic.AddUint32(&omb.header.Flags, 1)
	cmdCopy.Seq = seq

	// 写入命令副本到环形缓冲区
	omb.commands[currentWrite] = cmdCopy

	// 内存屏障：确保命令数据在指针更新前对其他CPU可见
	atomic.StoreUint32(&omb.header.Flags, seq)

	// 原子更新写指针和计数
	newWrite := (currentWrite + 1) % omb.config.Capacity
	atomic.StoreUint32(&omb.header.WriteIndex, newWrite)
	atomic.AddUint32(&omb.header.Count, 1)
	atomic.StoreUint64(&omb.header.Timestamp, cmdCopy.Timestamp)

	// 更新统计
	atomic.AddUint64(&omb.stats.totalWrites, 1)

	if omb.config.EnableLog {
		log.Printf("写入操作命令: Type=%d, SourceID=%d, Seq=%d",
			cmdCopy.Type, cmdCopy.SourceID, cmdCopy.Seq)
	}

	return true
}

// WriteCommands 批量写入命令（Unity端调用）
func (omb *OperationMemoryBlock) WriteCommands(cmds []OperationCommand) (successCount int) {
	available := omb.GetAvailableSpace()

	// 批量写入优化：如果可用空间足够，一次性写入多个
	if available >= len(cmds) {
		for i := range cmds {
			if omb.WriteCommand(&cmds[i]) {
				successCount++
			}
		}
	} else {
		// 可用空间不足，只写入能容纳的部分
		for i := 0; i < available; i++ {
			if omb.WriteCommand(&cmds[i]) {
				successCount++
			}
		}
	}

	return successCount
}

// GetAvailableSpace 获取可用空间（Go端可监控缓冲区状态）
func (omb *OperationMemoryBlock) GetAvailableSpace() int {
	currentCount := atomic.LoadUint32(&omb.header.Count)
	return int(omb.config.Capacity) - int(currentCount)
}

// IsFull 检查缓冲区是否已满
func (omb *OperationMemoryBlock) IsFull() bool {
	return atomic.LoadUint32(&omb.header.Count) >= omb.config.Capacity
}

// IsEmpty 检查缓冲区是否为空
func (omb *OperationMemoryBlock) IsEmpty() bool {
	return atomic.LoadUint32(&omb.header.Count) == 0
}

// GetWriteRate 估算Unity端写入速率（Go端监控用）
func (omb *OperationMemoryBlock) GetWriteRate() float64 {
	// 通过监控header.Timestamp变化估算写入频率
	// 实际实现需要记录时间窗口内的变化
	return 0.0 // 占位符，实际需要实现时间窗口统计
}

// GetStats 获取统计信息
func (omb *OperationMemoryBlock) GetStats() (reads, writes, overflow uint64) {
	// 对于Go端，totalWrites统计的是通过header.Timestamp监控到的Unity写入次数
	// 这是一个估算值，实际准确度取决于监控频率
	return atomic.LoadUint64(&omb.stats.totalReads),
		atomic.LoadUint64(&omb.stats.totalWrites),
		atomic.LoadUint64(&omb.stats.overflow)
}

// Clear 清空缓冲区（Unity端调用，重置状态）
func (omb *OperationMemoryBlock) Clear() {
	atomic.StoreUint32(&omb.header.ReadIndex, 0)
	atomic.StoreUint32(&omb.header.WriteIndex, 0)
	atomic.StoreUint32(&omb.header.Count, 0)
	atomic.StoreUint32(&omb.header.Flags, 0)
	atomic.StoreUint64(&omb.header.Timestamp, uint64(time.Now().UnixNano()))

	if omb.config.EnableLog {
		log.Printf("操作缓冲区已清空")
	}
}

// Close 关闭内存块（双方都应调用以释放资源）
func (omb *OperationMemoryBlock) Close() {
	if omb.shmPtr != 0 {
		procUnmapViewOfFile.Call(omb.shmPtr)
		omb.shmPtr = 0
	}
	if omb.hMapFile != 0 {
		procCloseHandle.Call(uintptr(omb.hMapFile))
		omb.hMapFile = 0
	}

	log.Printf("操作内存块 %s 已关闭", omb.config.Name)
}

// GetHealthStatus 获取内存块健康状态（Go端监控用）
func (omb *OperationMemoryBlock) GetHealthStatus() string {
	if omb.shmPtr == 0 {
		return "已关闭"
	}

	count := atomic.LoadUint32(&omb.header.Count)
	capacity := omb.config.Capacity
	utilization := float64(count) / float64(capacity) * 100

	switch {
	case utilization >= 90:
		return fmt.Sprintf("警告: 缓冲区接近满载 (%.1f%%)", utilization)
	case utilization >= 75:
		return fmt.Sprintf("正常: 缓冲区使用率较高 (%.1f%%)", utilization)
	default:
		return fmt.Sprintf("健康: 缓冲区使用率正常 (%.1f%%)", utilization)
	}
}
