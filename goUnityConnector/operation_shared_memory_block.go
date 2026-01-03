//go:build windows
// +build windows

package goUnityConnector

import (
	"fmt"
	"log"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	headerSize  = unsafe.Sizeof(OperationHeader{})
	commandSize = unsafe.Sizeof(OperationCommand{})
)

type OperationMemoryBlock struct {
	config       OperationMemoryBlockConfig
	hMapFile     syscall.Handle
	shmPtr       uintptr
	header       *OperationHeader
	commands     []OperationCommand // 指向共享内存中的环形缓冲区（仅引用）
	isOwner      bool               // 是否为创建者（Unity端通常为true）
	localReadSeq uint32

	// 统计信息（本地内存，非共享）
	stats struct {
		totalReads   uint64 // Go端读取次数
		totalWrites  uint64 // Unity端写入次数（通过监控header.Timestamp变化统计）
		overflow     uint64 // 缓冲区溢出次数
		seqWrapCount uint64
	}
}

// NewOperationMemoryBlock 创建或打开操作内存块
// create=true: 创建新共享内存（Unity端调用）
// create=false: 打开现有共享内存（Go端调用）
func NewOperationMemoryBlock(config OperationMemoryBlockConfig, create bool) (*OperationMemoryBlock, error) {
	if config.Capacity == 0 {
		return nil, fmt.Errorf("Capacity必须>0")
	}
	if config.Capacity > 0x7FFFFFFF {
		return nil, fmt.Errorf("Capacity过大，可能导致32位索引溢出")
	}
	// 计算总大小：头部 + 环形缓冲区

	totalSize := headerSize + uintptr(config.Capacity)*commandSize

	omb := &OperationMemoryBlock{
		config:       config,
		isOwner:      create,
		localReadSeq: 0,
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
	atomic.StoreUint32(&omb.header.Capacity, omb.config.Capacity)
	atomic.StoreUint32(&omb.header.ReadIndex, 0)  // Go读取位置
	atomic.StoreUint32(&omb.header.WriteIndex, 0) // Unity写入位置
	atomic.StoreUint32(&omb.header.MaxSeq, 0)

	if omb.config.EnableLog {
		log.Printf("操作内存块 %s 已初始化", omb.config.Name)
	}
}

// ReadCommands 读取命令（支持批量读取和单个读取）
// maxCount <= 0: 读取所有可用命令
// maxCount = 1: 读取单个命令（替代原ReadSingleCommand）
// 返回实际读取的命令列表，可能为空
func (omb *OperationMemoryBlock) ReadCommands(maxCount int) []OperationCommand {
	// 原子获取当前读写指针
	readIdx := atomic.LoadUint32(&omb.header.ReadIndex)
	writeIdx := atomic.LoadUint32(&omb.header.WriteIndex)
	capacity := atomic.LoadUint32(&omb.header.Capacity)

	// 计算可用命令数（处理索引环绕）
	var available int
	if writeIdx >= readIdx {
		available = int(writeIdx - readIdx)
	} else {
		available = int(capacity - readIdx + writeIdx)
	}

	if available == 0 {
		return nil
	}

	// 确定要读取的数量
	toRead := available
	if maxCount > 0 && maxCount < toRead {
		toRead = maxCount
	}

	// 预分配结果切片（旧格式）
	results := make([]OperationCommand, 0, toRead)
	readCount := 0

	// 逐个读取命令
	for i := 0; i < toRead; i++ {
		index := (readIdx + uint32(i)) % capacity
		cmd := omb.commands[index]

		// 序列号验证（允许环绕）
		if omb.localReadSeq != 0 {
			// 检测序列号环绕：newSeq < oldSeq 且差值较大
			diff := int32(cmd.Seq - omb.localReadSeq)
			if diff < 0 && -diff > 0x7FFFFFFF {
				// 有效环绕
				atomic.AddUint64(&omb.stats.seqWrapCount, 1)
			} else if diff != 1 && diff != 0 {
				// 序列号不连续（可能数据撕裂或丢失）
				if omb.config.EnableLog {
					log.Printf("警告: 序列号不连续 at index %d (前序=%d, 实际=%d, 差值=%d)",
						index, omb.localReadSeq, cmd.Seq, diff)
				}
				// 继续读取，不中断（简化处理）
			}
		}

		results = append(results, cmd)
		readCount++
		omb.localReadSeq = cmd.Seq
	}

	if readCount == 0 {
		return nil
	}

	// 原子更新读指针
	newRead := (readIdx + uint32(readCount)) % capacity
	atomic.StoreUint32(&omb.header.ReadIndex, newRead)

	// 更新统计
	atomic.AddUint64(&omb.stats.totalReads, uint64(readCount))

	// 日志输出
	if omb.config.EnableLog && readCount > 0 {
		log.Printf("读取命令: 数量=%d, 首命令类型=%v, 首命令Seq=%d",
			readCount, results[0].Type, results[0].Seq)
	}

	return results
}

// Clear 清空缓冲区（Unity端调用，重置状态）
func (omb *OperationMemoryBlock) Clear() {
	// 紧凑头部只有4个字段，重置它们
	atomic.StoreUint32(&omb.header.ReadIndex, 0)
	atomic.StoreUint32(&omb.header.WriteIndex, 0)
	atomic.StoreUint32(&omb.header.MaxSeq, 0)

	// 同时重置本地状态
	omb.localReadSeq = 0

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
