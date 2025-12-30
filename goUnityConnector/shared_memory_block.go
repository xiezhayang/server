// shared_memory_block.go
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

// Windows共享内存常量
var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateFileMappingW = modkernel32.NewProc("CreateFileMappingW")
	procMapViewOfFile      = modkernel32.NewProc("MapViewOfFile")
	procUnmapViewOfFile    = modkernel32.NewProc("UnmapViewOfFile")
	procCloseHandle        = modkernel32.NewProc("CloseHandle")
)

const (
	PAGE_READWRITE       = 0x04
	FILE_MAP_ALL_ACCESS  = 0xF001F
	INVALID_HANDLE_VALUE = ^syscall.Handle(0)

	// 内存布局常量
	HeaderSize     = unsafe.Sizeof(SyncHeader{})
	ObjectDataSize = unsafe.Sizeof(ObjectSyncData{})
)

// MemoryBlock 固定数组+动态头部的共享内存块
type extensiveMemoryBlock struct {
	config      extensiveMemoryBlockConfig
	hMapFile    syscall.Handle
	shmPtr      uintptr
	header      *SyncHeader
	objectData  []ObjectSyncData // 指向共享内存中的数组
	isOwner     bool
	bufferIndex int32 // 当前活动缓冲区索引（双缓冲时使用）

	// 统计信息
	stats struct {
		totalWrites uint64
		totalBytes  uint64
	}
}

// OperationMemoryBlock 操作内存块
type OperationMemoryBlock struct {
	config      OperationMemoryBlockConfig
	hMapFile    syscall.Handle
	shmPtr      uintptr
	header      *OperationHeader
	commands    []OperationCommand // 指向共享内存中的环形缓冲区
	isOwner     bool
	bufferIndex int32 // 当前活动写缓冲区索引（与BackBuffer同步）

	// 统计信息
	stats struct {
		totalWrites uint64
		totalReads  uint64
		overflow    uint64 // 缓冲区溢出次数
	}
}

// NewExtensiveMemoryBlock 创建新的共享内存块
func NewExtensiveMemoryBlock(config extensiveMemoryBlockConfig, create bool) (*extensiveMemoryBlock, error) {
	// 验证配置
	if config.MaxObjects == 0 {
		return nil, fmt.Errorf("MaxObjects must be > 0")
	}

	// 计算总大小
	totalSize := HeaderSize + (uintptr(config.MaxObjects) * ObjectDataSize)
	if config.EnableDoubleBuffer {
		totalSize *= 2 // 双倍大小用于双缓冲
	}

	mb := &extensiveMemoryBlock{
		config:  config,
		isOwner: create,
	}

	// 转换名称为UTF-16
	namePtr, err := syscall.UTF16PtrFromString(config.Name)
	if err != nil {
		return nil, fmt.Errorf("名称转换失败: %v", err)
	}

	if create {
		// 创建共享内存
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
		mb.hMapFile = syscall.Handle(hMap)
	} else {
		// 打开现有共享内存
		hMap, _, err := procCreateFileMappingW.Call(
			uintptr(INVALID_HANDLE_VALUE),
			0,
			uintptr(PAGE_READWRITE),
			0,
			uintptr(totalSize),
			uintptr(unsafe.Pointer(namePtr)),
		)

		if hMap == 0 {
			return nil, fmt.Errorf("打开共享内存失败: %v", err)
		}
		mb.hMapFile = syscall.Handle(hMap)
	}

	// 映射共享内存视图
	ptr, _, err := procMapViewOfFile.Call(
		uintptr(mb.hMapFile),
		uintptr(FILE_MAP_ALL_ACCESS),
		0,
		0,
		uintptr(totalSize),
	)

	if ptr == 0 {
		procCloseHandle.Call(uintptr(mb.hMapFile))
		return nil, fmt.Errorf("映射共享内存失败: %v", err)
	}

	mb.shmPtr = ptr

	// 设置指针
	if config.EnableDoubleBuffer {
		// 双缓冲：每个缓冲区有自己的头部和数据
		bufferSize := HeaderSize + (uintptr(config.MaxObjects) * ObjectDataSize)
		currentBuffer := atomic.LoadInt32(&mb.bufferIndex)
		basePtr := ptr + uintptr(currentBuffer)*bufferSize

		mb.header = (*SyncHeader)(unsafe.Pointer(basePtr))
		dataStart := basePtr + HeaderSize
		// 创建切片指向共享内存中的数组
		sliceHeader := &struct {
			data uintptr
			len  int
			cap  int
		}{
			data: dataStart,
			len:  int(config.MaxObjects),
			cap:  int(config.MaxObjects),
		}
		mb.objectData = *(*[]ObjectSyncData)(unsafe.Pointer(sliceHeader))
	} else {
		// 单缓冲
		mb.header = (*SyncHeader)(unsafe.Pointer(ptr))
		dataStart := ptr + HeaderSize
		sliceHeader := &struct {
			data uintptr
			len  int
			cap  int
		}{
			data: dataStart,
			len:  int(config.MaxObjects),
			cap:  int(config.MaxObjects),
		}
		mb.objectData = *(*[]ObjectSyncData)(unsafe.Pointer(sliceHeader))
	}

	// 如果是创建者，初始化头部
	if create {
		mb.Initialize()
	}

	log.Printf("共享内存块 %s 已%s (容量: %d 对象, 大小: %d 字节)",
		config.Name, map[bool]string{true: "创建", false: "打开"}[create],
		config.MaxObjects, totalSize)

	return mb, nil
}

// NewOperationMemoryBlock 创建新的操作内存块
// NewOperationMemoryBlock 创建新的操作内存块
func NewOperationMemoryBlock(config OperationMemoryBlockConfig, create bool) (*OperationMemoryBlock, error) {
	if config.Capacity == 0 {
		return nil, fmt.Errorf("Capacity必须>0")
	}

	// 计算总大小
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
		// 创建共享内存
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
		// 打开现有共享内存
		hMap, _, err := procCreateFileMappingW.Call(
			uintptr(INVALID_HANDLE_VALUE),
			0,
			uintptr(PAGE_READWRITE),
			0,
			uintptr(totalSize),
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

	// 创建切片指向环形缓冲区
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

	// 如果是创建者，初始化头部
	if create {
		omb.Initialize()
	}

	log.Printf("操作内存块 %s 已%s (容量: %d 命令, 大小: %d 字节)",
		config.Name, map[bool]string{true: "创建", false: "打开"}[create],
		config.Capacity, totalSize)

	return omb, nil
}

// Initialize 初始化共享内存头部
// Initialize 初始化共享内存头部
func (mb *extensiveMemoryBlock) Initialize() {
	// 使用内存屏障确保写入顺序
	atomic.StoreUint32(&mb.header.Version, 1)
	atomic.StoreUint32(&mb.header.MaxObjects, mb.config.MaxObjects)
	atomic.StoreUint64(&mb.header.FrameNumber, 0)
	atomic.StoreUint32(&mb.header.ChangedCount, 0)
	atomic.StoreUint32(&mb.header.TotalCount, 0)
	atomic.StoreUint32(&mb.header.Flags, 0)

	// 初始化双缓冲指针
	if mb.config.EnableDoubleBuffer {
		atomic.StoreUint32(&mb.header.FrontBuffer, 0)
		atomic.StoreUint32(&mb.header.BackBuffer, 1)
		mb.bufferIndex = 1 // Go初始写入BackBuffer(1)

		// 初始化另一个缓冲区的头部
		bufferSize := HeaderSize + (uintptr(mb.config.MaxObjects) * ObjectDataSize)
		otherHeader := (*SyncHeader)(unsafe.Pointer(mb.shmPtr + bufferSize))

		atomic.StoreUint32(&otherHeader.Version, 1)
		atomic.StoreUint32(&otherHeader.MaxObjects, mb.config.MaxObjects)
		atomic.StoreUint64(&otherHeader.FrameNumber, 0)
		atomic.StoreUint32(&otherHeader.ChangedCount, 0)
		atomic.StoreUint32(&otherHeader.TotalCount, 0)
		atomic.StoreUint32(&otherHeader.Flags, 0)
		atomic.StoreUint32(&otherHeader.FrontBuffer, 0)
		atomic.StoreUint32(&otherHeader.BackBuffer, 1)
	} else {
		atomic.StoreUint32(&mb.header.FrontBuffer, 0)
		atomic.StoreUint32(&mb.header.BackBuffer, 0)
		mb.bufferIndex = 0
	}
}

// Initialize 初始化操作内存块
func (omb *OperationMemoryBlock) Initialize() {
	atomic.StoreUint32(&omb.header.Version, 1)
	atomic.StoreUint32(&omb.header.Capacity, omb.config.Capacity)
	atomic.StoreUint32(&omb.header.ReadIndex, 0)
	atomic.StoreUint32(&omb.header.WriteIndex, 0)
	atomic.StoreUint32(&omb.header.Count, 0)
	atomic.StoreUint32(&omb.header.Flags, 0)
	atomic.StoreUint64(&omb.header.Timestamp, uint64(time.Now().UnixNano()))
}

// SwapBuffer 切换双缓冲区（如果是双缓冲模式）
func (mb *extensiveMemoryBlock) SwapBuffer() (oldIndex, newIndex int32) {
	if !mb.config.EnableDoubleBuffer {
		return 0, 0
	}

	oldIndex = atomic.LoadInt32(&mb.bufferIndex)
	newIndex = 1 - oldIndex

	// 交换FrontBuffer和BackBuffer指针
	oldFront := atomic.LoadUint32(&mb.header.FrontBuffer)
	oldBack := atomic.LoadUint32(&mb.header.BackBuffer)

	// 更新当前头部（即将变为非活动状态）
	atomic.StoreUint32(&mb.header.FrontBuffer, oldBack)
	atomic.StoreUint32(&mb.header.BackBuffer, oldFront)

	// 更新另一个头部的指针
	bufferSize := HeaderSize + (uintptr(mb.config.MaxObjects) * ObjectDataSize)
	otherHeaderPtr := mb.shmPtr + uintptr(newIndex)*bufferSize
	otherHeader := (*SyncHeader)(unsafe.Pointer(otherHeaderPtr))
	atomic.StoreUint32(&otherHeader.FrontBuffer, oldBack)
	atomic.StoreUint32(&otherHeader.BackBuffer, oldFront)

	// 切换本地缓冲区索引
	atomic.StoreInt32(&mb.bufferIndex, newIndex)

	// 更新本地指针
	basePtr := mb.shmPtr + uintptr(newIndex)*bufferSize
	mb.header = (*SyncHeader)(unsafe.Pointer(basePtr))
	dataStart := basePtr + HeaderSize

	sliceHeader := &struct {
		data uintptr
		len  int
		cap  int
	}{
		data: dataStart,
		len:  int(mb.config.MaxObjects),
		cap:  int(mb.config.MaxObjects),
	}
	mb.objectData = *(*[]ObjectSyncData)(unsafe.Pointer(sliceHeader))

	return oldIndex, newIndex
}

// WriteObject 写入单个对象数据（增量更新）
func (mb *extensiveMemoryBlock) WriteObject(objID uint32, data *ObjectSyncData) bool {
	if objID >= mb.config.MaxObjects {
		log.Printf("错误: ObjectID %d 超出范围 (最大: %d)", objID, mb.config.MaxObjects-1)
		return false
	}

	// 写入对象数据
	mb.objectData[objID] = *data

	// 更新统计
	atomic.AddUint64(&mb.stats.totalWrites, 1)
	atomic.AddUint64(&mb.stats.totalBytes, uint64(ObjectDataSize))

	return true
}

// WriteObjects 批量写入对象数据
func (mb *extensiveMemoryBlock) WriteObjects(objects []ObjectSyncData, changed []uint32) {
	// 写入变化的对象
	for i, objID := range changed {
		if objID < mb.config.MaxObjects {
			mb.objectData[objID] = objects[i]
		}
	}

	// 更新统计
	atomic.AddUint64(&mb.stats.totalWrites, uint64(len(changed)))
	atomic.AddUint64(&mb.stats.totalBytes, uint64(len(changed))*uint64(ObjectDataSize))
}

// UpdateHeader 更新内存块头部信息
func (mb *extensiveMemoryBlock) UpdateHeader(frameNum uint64, changedCount, totalCount uint32, flags uint32) {
	atomic.StoreUint64(&mb.header.FrameNumber, frameNum)
	atomic.StoreUint32(&mb.header.ChangedCount, changedCount)
	atomic.StoreUint32(&mb.header.TotalCount, totalCount)
	atomic.StoreUint32(&mb.header.Flags, flags)
	atomic.StoreUint64(&mb.header.Timestamp, uint64(time.Now().UnixNano()))
}

// GetStats 获取统计信息
func (mb *extensiveMemoryBlock) GetStats() (writes, bytes uint64) {
	return atomic.LoadUint64(&mb.stats.totalWrites), atomic.LoadUint64(&mb.stats.totalBytes)
}

// Close 关闭共享内存
func (mb *extensiveMemoryBlock) Close() {
	if mb.shmPtr != 0 {
		procUnmapViewOfFile.Call(mb.shmPtr)
		mb.shmPtr = 0
	}
	if mb.hMapFile != 0 {
		procCloseHandle.Call(uintptr(mb.hMapFile))
		mb.hMapFile = 0
	}

	log.Printf("共享内存块 %s 已关闭", mb.config.Name)
}

// ReadCommands 读取所有可用命令
// ReadCommands 读取所有可用命令（无锁）
func (omb *OperationMemoryBlock) ReadCommands(maxCount int) []OperationCommand {
	currentRead := atomic.LoadUint32(&omb.header.ReadIndex)
	currentCount := atomic.LoadUint32(&omb.header.Count)

	if currentCount == 0 {
		return nil
	}

	// 限制读取数量
	readCount := int(currentCount)
	if maxCount > 0 && readCount > maxCount {
		readCount = maxCount
	}

	// 预分配结果切片
	commands := make([]OperationCommand, readCount)

	// 从环形缓冲区复制数据
	for i := 0; i < readCount; i++ {
		idx := (currentRead + uint32(i)) % omb.config.Capacity
		commands[i] = omb.commands[idx]
	}

	// 原子更新读指针和计数
	if readCount > 0 {
		newRead := (currentRead + uint32(readCount)) % omb.config.Capacity
		atomic.StoreUint32(&omb.header.ReadIndex, newRead)
		atomic.AddUint32(&omb.header.Count, ^uint32(readCount-1)) // 原子减 readCount
		atomic.AddUint64(&omb.stats.totalReads, uint64(readCount))
	}

	return commands
}

// ReadSingleCommand 读取单个命令（如果可用）
func (omb *OperationMemoryBlock) ReadSingleCommand() (*OperationCommand, bool) {
	if atomic.LoadUint32(&omb.header.Count) == 0 {
		return nil, false
	}

	currentRead := atomic.LoadUint32(&omb.header.ReadIndex)
	cmd := omb.commands[currentRead]

	// 更新指针和计数
	newRead := (currentRead + 1) % omb.config.Capacity
	atomic.StoreUint32(&omb.header.ReadIndex, newRead)
	atomic.AddUint32(&omb.header.Count, ^uint32(0)) // 等价于原子减1

	atomic.AddUint64(&omb.stats.totalReads, 1)
	return &cmd, true
}

// GetAvailableSpace 获取可用空间

// WriteCommand 写入单个操作命令（线程安全，无锁）
func (omb *OperationMemoryBlock) WriteCommand(cmd *OperationCommand) bool {
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

	// 设置时间戳
	cmd.Timestamp = uint64(time.Now().UnixNano())

	// 写入命令到环形缓冲区
	omb.commands[currentWrite] = *cmd

	// 原子更新写指针和计数（必须保持顺序）
	newWrite := (currentWrite + 1) % omb.config.Capacity
	atomic.StoreUint32(&omb.header.WriteIndex, newWrite)
	atomic.AddUint32(&omb.header.Count, 1)
	atomic.StoreUint64(&omb.header.Timestamp, cmd.Timestamp)

	// 更新统计
	atomic.AddUint64(&omb.stats.totalWrites, 1)

	if omb.config.EnableLog {
		log.Printf("写入操作命令: Type=%d, SourceID=%d", cmd.Type, cmd.SourceID)
	}

	return true
}

// WriteCommands 批量写入操作命令（优化版本）
func (omb *OperationMemoryBlock) WriteCommands(cmds []OperationCommand) (successCount int) {
	for i := range cmds {
		if omb.WriteCommand(&cmds[i]) {
			successCount++
		}
	}
	return successCount
}

// GetAvailableSpace 获取可用空间
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

// GetStats 获取统计信息
func (omb *OperationMemoryBlock) GetStats() (writes, reads, overflow uint64) {
	return atomic.LoadUint64(&omb.stats.totalWrites),
		atomic.LoadUint64(&omb.stats.totalReads),
		atomic.LoadUint64(&omb.stats.overflow)
}

// Clear 清空缓冲区
func (omb *OperationMemoryBlock) Clear() {
	atomic.StoreUint32(&omb.header.ReadIndex, 0)
	atomic.StoreUint32(&omb.header.WriteIndex, 0)
	atomic.StoreUint32(&omb.header.Count, 0)
	atomic.StoreUint64(&omb.header.Timestamp, uint64(time.Now().UnixNano()))
}

// Close 关闭内存块
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
