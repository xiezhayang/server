// shared_memory_block.go
//go:build windows
// +build windows

package goUnityConnector

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Windows共享内存常量
var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateFileMappingW = modkernel32.NewProc("CreateFileMappingW")
	procOpenFileMappingW   = modkernel32.NewProc("OpenFileMappingW")
	procMapViewOfFile      = modkernel32.NewProc("MapViewOfFile")
	procUnmapViewOfFile    = modkernel32.NewProc("UnmapViewOfFile")
	procCloseHandle        = modkernel32.NewProc("CloseHandle")
)

const (
	PAGE_READWRITE       = 0x04
	FILE_MAP_ALL_ACCESS  = 0xF001F
	INVALID_HANDLE_VALUE = ^syscall.Handle(0)

	// 内存布局常量
	extensiveHeaderSize  = unsafe.Sizeof(extensiveHeader{})
	objectDataSize       = unsafe.Sizeof(ObjectSyncData{})
	operationHeaderSize  = unsafe.Sizeof(OperationHeader{})
	operationCommandSize = unsafe.Sizeof(OperationCommand{})
)

type extensiveMemoryBlockManager struct {
	config          extensiveMemoryBlockConfig
	header          extensiveHeader
	shmPtr          uintptr
	blocks          map[uint64]*extensiveMemoryBlock
	currentWriteIdx uint32
	lock            sync.Mutex
	nextBlockID     uint64
	// 统计信息
	stats struct {
		totalWrites    uint64
		totalBytes     uint64
		extensions     uint64 // 扩展次数
		maxObjectsEver uint32 // 历史最大对象数
	}
}

type extensiveMemoryBlock struct {
	hMapFile    syscall.Handle
	dataBuffers [2][]ObjectSyncData
	blockID     uint64 // 块链唯一标识
}

func validateConfig(config extensiveMemoryBlockConfig) error {
	// 必需字段检查
	if config.Name == "" {
		return errors.New("内存块名称不能为空")
	}

	if config.MaxObjects == 0 {
		return errors.New("最大对象数必须大于0")
	}

	// 扩展策略一致性检查
	if config.EnableAutoExtend {
		if config.ExtendStrategy != "fixed" &&
			config.ExtendStrategy != "percentage" &&
			config.ExtendStrategy != "double" {
			return fmt.Errorf("无效的扩展策略: %s", config.ExtendStrategy)
		}

		if config.ExtendStrategy == "fixed" && config.ExtendSize == 0 {
			return errors.New("固定扩展策略下，扩展大小必须大于0")
		}

		if config.ExtendStrategy == "percentage" &&
			(config.ExtendPercentage <= 0 || config.ExtendPercentage > 1) {
			return fmt.Errorf("百分比扩展必须在 (0,1] 范围内: %f", config.ExtendPercentage)
		}
	}

	return nil
}

func NewExtensiveMemoryBlockManager(config extensiveMemoryBlockConfig) (*extensiveMemoryBlockManager, error) {
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("配置验证失败: %w", err) // %w 包装错误，保留原始信息
	}

	// 2. 检查资源限制（例如 Windows 共享内存大小限制）
	maxAllowedSize := uintptr(config.MaxObjects) * objectDataSize
	if config.EnableDoubleBuffer {
		maxAllowedSize *= 2
	}
	if maxAllowedSize > 1024*1024*1024 { // 1GB 限制示例
		return nil, fmt.Errorf("请求的内存大小超过限制: %d 字节", maxAllowedSize)
	}
	embm := &extensiveMemoryBlockManager{
		config:          config,
		header:          extensiveHeader{},
		blocks:          make(map[uint64]*extensiveMemoryBlock),
		currentWriteIdx: 0,
		lock:            sync.Mutex{},
		nextBlockID:     0,
		stats: struct {
			totalWrites    uint64
			totalBytes     uint64
			extensions     uint64
			maxObjectsEver uint32
		}{
			totalWrites:    0,
			totalBytes:     0,
			extensions:     0,
			maxObjectsEver: 0,
		},
	}
	embm.shmPtr = uintptr(unsafe.Pointer(&embm.header))
	return embm, nil

}

func (embm *extensiveMemoryBlockManager) NewExtensiveMemoryBlock(blockID uint64) error {
	// 验证配置
	embm.lock.Lock()
	defer embm.lock.Unlock()
	if embm.config.MaxObjects == 0 {
		return fmt.Errorf("MaxObjects must be > 0")
	}
	if blockID == 0 {
		blockID = embm.nextBlockID
		embm.nextBlockID++
	}
	// 计算总大小：头部 + 数据缓冲区（单缓冲1个，双缓冲2个）
	bufferDataSize := uintptr(embm.config.MaxObjects) * objectDataSize
	if embm.config.EnableDoubleBuffer {
		bufferDataSize = 2 * bufferDataSize // 双缓冲：两个数据区
	}
	name := fmt.Sprintf("%s_%d", embm.config.Name, blockID)

	// 转换名称为UTF-16
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("名称转换失败: %v", err)
	}
	// 创建共享内存
	hMap, _, err := procCreateFileMappingW.Call(
		uintptr(INVALID_HANDLE_VALUE), // 使用系统页面文件
		0,                             // 安全属性
		uintptr(PAGE_READWRITE),       // 保护模式
		0,                             // 高位大小
		uintptr(bufferDataSize),       // 低位大小
		uintptr(unsafe.Pointer(namePtr)),
	)

	if hMap == 0 {
		return fmt.Errorf("创建共享内存失败: %v", err)
	}
	mb := &extensiveMemoryBlock{
		hMapFile:    syscall.Handle(hMap),
		blockID:     blockID,
		dataBuffers: [2][]ObjectSyncData{},
	}
	// 映射共享内存视图
	ptr, _, err := procMapViewOfFile.Call(
		uintptr(mb.hMapFile),
		uintptr(FILE_MAP_ALL_ACCESS),
		0,
		0,
		uintptr(bufferDataSize),
	)

	if ptr == 0 {
		procCloseHandle.Call(uintptr(mb.hMapFile))
		return fmt.Errorf("映射共享内存失败: %v", err)
	}

	// 缓冲区0（始终存在）
	sliceHeader0 := &struct {
		data uintptr
		len  int
		cap  int
	}{
		data: ptr,
		len:  int(embm.config.MaxObjects),
		cap:  int(embm.config.MaxObjects),
	}
	mb.dataBuffers[0] = *(*[]ObjectSyncData)(unsafe.Pointer(sliceHeader0))

	// 缓冲区1（仅双缓冲时有效）
	if embm.config.EnableDoubleBuffer {
		sliceHeader1 := &struct {
			data uintptr
			len  int
			cap  int
		}{
			data: ptr + bufferDataSize,
			len:  int(embm.config.MaxObjects),
			cap:  int(embm.config.MaxObjects),
		}
		mb.dataBuffers[1] = *(*[]ObjectSyncData)(unsafe.Pointer(sliceHeader1))
	} else {
		// 单缓冲：缓冲区1设为nil，避免混淆
		mb.dataBuffers[1] = nil
	}
	embm.blocks[blockID] = mb
	// 修正日志消息
	bufferText := "对象"
	if embm.config.EnableDoubleBuffer {
		bufferText = "对象×2"
	}
	log.Printf("单头部共享内存块 %s 已%s (容量: %d %s, 大小: %d 字节)",
		embm.config.Name, map[bool]string{true: "创建", false: "打开"}[true],
		embm.config.MaxObjects, bufferText, bufferDataSize)

	return nil
}

// GetCurrentWriteBuffer 获取当前写入缓冲区的切片（带单缓冲安全检查）
func (embm *extensiveMemoryBlockManager) GetCurrentWriteBuffer() uint32 {
	idx := atomic.LoadUint32(&embm.currentWriteIdx)
	// 单缓冲模式下，索引1应重定向到索引0
	if idx == 1 && !embm.config.EnableDoubleBuffer {
		return 0
	}
	return idx
}

// GetFrontBufferData 获取FrontBuffer指向的数据（用于调试或监控）

// SwapBuffer 交换双缓冲区（简化版）
func (embm *extensiveMemoryBlockManager) SwapBuffer() (oldFront, newFront uint32) {
	if !embm.config.EnableDoubleBuffer {
		return 0, 0
	}

	// 原子交换FrontBuffer和BackBuffer
	currentFront := atomic.LoadUint32(&embm.header.FrontBuffer)
	currentBack := atomic.LoadUint32(&embm.header.BackBuffer)

	// 交换：FrontBuffer←BackBuffer, BackBuffer←FrontBuffer
	atomic.StoreUint32(&embm.header.FrontBuffer, currentBack)
	atomic.StoreUint32(&embm.header.BackBuffer, currentFront)

	// 更新本地写入索引（与新的BackBuffer同步）
	atomic.StoreUint32(&embm.currentWriteIdx, uint32(currentFront))

	return currentFront, currentBack
}

// UpdateHeader 更新内存块头部信息

func (embm *extensiveMemoryBlockManager) UpdateHeader(frameNum uint64, changedCount, totalObjects uint32) {
	// 使用原子操作确保写入顺序和可见性
	atomic.StoreUint64(&embm.header.FrameNumber, frameNum)
	atomic.StoreUint32(&embm.header.ChangedCount, changedCount)
	atomic.StoreUint32(&embm.header.TotalObjects, totalObjects)
	//atomic.StoreUint32(&embm.header.Flags, flags)
	atomic.StoreUint64(&embm.header.Timestamp, uint64(time.Now().UnixNano()))
}

// GetStats 获取统计信息

func (embm *extensiveMemoryBlockManager) GetStats() (writes, bytes uint64) {
	return atomic.LoadUint64(&embm.stats.totalWrites), atomic.LoadUint64(&embm.stats.totalBytes)
}

// Close 关闭共享内存
func (embm *extensiveMemoryBlockManager) Close() {
	if embm.shmPtr != 0 {
		procUnmapViewOfFile.Call(embm.shmPtr)
		embm.shmPtr = 0
	}
	var name string
	for _, block := range embm.blocks {
		procCloseHandle.Call(uintptr(block.hMapFile))
		block.hMapFile = 0
		name = fmt.Sprintf("%s_%d", embm.config.Name, block.blockID)
		log.Printf("共享内存块 %s 已关闭", name)
	}

}

// GetBufferInfo 获取缓冲区信息（调试用）
func (embm *extensiveMemoryBlockManager) GetBufferInfo() (frontIdx, backIdx uint32, localWriteIdx uint32) {
	return atomic.LoadUint32(&embm.header.FrontBuffer),
		atomic.LoadUint32(&embm.header.BackBuffer),
		atomic.LoadUint32(&embm.currentWriteIdx)

}

// IsDoubleBufferEnabled 检查是否启用双缓冲
func (embm *extensiveMemoryBlockManager) IsDoubleBufferEnabled() bool {
	return embm.config.EnableDoubleBuffer
}

// GetObjectCount 获取当前有效对象数量
func (embm *extensiveMemoryBlockManager) GetObjectCount() uint32 {
	return atomic.LoadUint32(&embm.header.TotalObjects)
}

// GetFrameNumber 获取当前帧号
func (embm *extensiveMemoryBlockManager) GetFrameNumber() uint64 {
	return atomic.LoadUint64(&embm.header.FrameNumber)
}

// 当前实现
func (mb *extensiveMemoryBlock) GetFrontBufferData() []ObjectSyncData {
	frontIdx := atomic.LoadUint32(&mb.header.FrontBuffer)
	return mb.dataBuffers[frontIdx] // ← 单缓冲时若 frontIdx=1 会返回 nil
}

func (embm *extensiveMemoryBlockManager) allocateObjectID() (uint32, bool) {
	// 原子获取当前总对象数
	totalCount := atomic.LoadUint32(&mb.header.TotalCount)

	// 检查当前块是否有空间
	if totalCount < mb.config.MaxObjects {
		// 在当前块分配
		newID := totalCount
		atomic.AddUint32(&mb.header.TotalCount, 1)
		return newID, true
	}

	// 当前块已满，需要扩展
	return mb.extendAndAllocate()
}

// getBlockByName 从缓存获取或打开共享内存块
func getBlockByName(name string) *extensiveMemoryBlock {
	blockCacheMu.RLock()
	if block, ok := blockCache[name]; ok {
		blockCacheMu.RUnlock()
		return block
	}
	blockCacheMu.RUnlock()

	// 打开现有块（非创建者）
	config := extensiveMemoryBlockConfig{
		Name:               name,
		MaxObjects:         0, // 打开时不需要指定大小
		EnableDoubleBuffer: false,
		EnableAutoExtend:   false,
	}

	block, err := NewExtensiveMemoryBlock(config, false)
	if err != nil {
		log.Printf("警告: 无法打开块 %s: %v", name, err)
		return nil
	}

	blockCacheMu.Lock()
	blockCache[name] = block
	blockCacheMu.Unlock()

	return block
}

// max 返回两个uint32中的较大值
func max(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

// calculateTotalCapacity 计算整个块链的总容量
func (mb *extensiveMemoryBlock) calculateTotalCapacity() uint32 {
	// 如果当前块是头块且header.MaxObjects已更新为总容量，直接返回
	if mb.isHeadBlock {
		if total := atomic.LoadUint32(&mb.header.MaxObjects); total > 0 {
			return total
		}
	}

	// 否则遍历链表计算
	total := uint32(0)
	current := mb
	for current != nil {
		total += current.config.MaxObjects
		if current.header.HasNext == 0 {
			break
		}
		// 注意：这里可能递归调用getBlockByName，需要防止循环引用
		nextName := string(current.header.NextBlockName[:])
		current = getBlockByName(nextName)
	}
	return total
}

// generateBlockName 生成唯一块名称
func generateBlockName(baseName string, extensionSize uint32) string {
	timestamp := time.Now().UnixNano()
	return fmt.Sprintf("%s_ext_%d_%d", baseName, extensionSize, timestamp)
}

// cleanupBlockChain 清理整个块链（供Close调用）
func (mb *extensiveMemoryBlock) cleanupBlockChain() {
	current := mb
	for current != nil {
		nextName := ""
		if current.header.HasNext == 1 {
			nextName = string(current.header.NextBlockName[:])
		}

		// 关闭当前块
		if current.shmPtr != 0 {
			procUnmapViewOfFile.Call(current.shmPtr)
			current.shmPtr = 0
		}
		if current.hMapFile != 0 {
			procCloseHandle.Call(uintptr(current.hMapFile))
			current.hMapFile = 0
		}

		// 从缓存移除
		blockCacheMu.Lock()
		delete(blockCache, current.config.Name)
		blockCacheMu.Unlock()

		// 移动到下一块
		if nextName != "" {
			current = getBlockByName(nextName)
		} else {
			current = nil
		}
	}
}

func (mb *extensiveMemoryBlock) extendAndAllocate() (uint32, bool) {
	mb.extendLock.Lock()
	defer mb.extendLock.Unlock()

	currentTotalCapacity := mb.calculateTotalCapacity()

	// 根据配置选择扩展策略
	var extensionSize uint32
	switch mb.config.ExtendStrategy {
	case "fixed":
		extensionSize = mb.config.ExtendSize
	case "percentage":
		extensionSize = uint32(float64(currentTotalCapacity) * mb.config.ExtendPercentage)
	case "double":
		extensionSize = currentTotalCapacity
	default: // "percentage" 作为默认
		extensionSize = max(currentTotalCapacity/2, 256)
	}

	// 检查最大限制
	if mb.config.MaxTotalObjects > 0 && currentTotalCapacity+extensionSize > mb.config.MaxTotalObjects {
		if currentTotalCapacity >= mb.config.MaxTotalObjects {
			log.Printf("错误: 已达到最大对象数限制 %d", mb.config.MaxTotalObjects)
			return 0, false
		}
		extensionSize = mb.config.MaxTotalObjects - currentTotalCapacity
	}

	// 创建新块
	newConfig := mb.config
	newConfig.Name = generateBlockName(mb.config.Name, extensionSize)
	newConfig.MaxObjects = extensionSize
	newConfig.EnableAutoExtend = mb.config.EnableAutoExtend // 继承自动扩展设置

	newBlock, err := NewExtensiveMemoryBlock(newConfig, true)
	if err != nil {
		log.Printf("扩展失败: %v", err)
		return 0, false
	}

	// 设置双向链表指针
	newBlock.prevBlock = mb
	newBlock.blockChainID = mb.blockChainID
	newBlock.localBlockIdx = mb.localBlockIdx + 1
	newBlock.isHeadBlock = false

	// 原子连接块链
	for {
		if atomic.CompareAndSwapUint32(&mb.header.HasNext, 0, 1) {
			// 设置下一块名称
			copy(mb.header.NextBlockName[:], newConfig.Name)
			mb.header.NextBlockName[63] = 0 // 确保终止

			// 更新头块的总容量（仅头块存储总容量）
			if mb.isHeadBlock {
				newTotalCapacity := currentTotalCapacity + extensionSize
				atomic.StoreUint32(&mb.header.MaxObjects, newTotalCapacity)
			}

			// 更新统计
			atomic.AddUint64(&mb.stats.extensions, 1)
			atomic.StoreUint32(&mb.stats.maxObjectsEver, currentTotalCapacity+extensionSize)

			// 在新块中分配
			newID := currentTotalCapacity // 第一个ID是原总容量
			atomic.AddUint32(&newBlock.header.TotalCount, 1)

			// 缓存新块
			blockCacheMu.Lock()
			blockCache[newConfig.Name] = newBlock
			blockCacheMu.Unlock()

			return newID, true
		}
		// 其他线程已扩展，重试分配
		return mb.allocateObjectID()
	}
}

// WriteObject 写入单个对象数据（支持扩展块）
func (mb *extensiveMemoryBlock) WriteObject(objID uint32, data *ObjectSyncData) bool {
	// 定位对象所在的块
	targetBlock, localIdx := mb.locateObject(objID)
	if targetBlock == nil {
		log.Printf("错误: ObjectID %d 超出所有块范围", objID)
		return false
	}

	// 获取目标块的当前写入缓冲区
	writeBuffer := targetBlock.GetCurrentWriteBuffer()
	if localIdx >= uint32(len(writeBuffer)) {
		log.Printf("错误: 局部索引 %d 超出缓冲区大小 %d", localIdx, len(writeBuffer))
		return false
	}

	writeBuffer[localIdx] = *data

	// 更新统计
	atomic.AddUint64(&mb.stats.totalWrites, 1)
	atomic.AddUint64(&mb.stats.totalBytes, uint64(objectDataSize))

	return true
}

// WriteObjects 批量写入对象数据（支持扩展块）
func (mb *extensiveMemoryBlock) WriteObjects(objects []ObjectSyncData, changed []uint32) {
	if len(objects) == 0 || len(changed) == 0 {
		return
	}

	actualWrites := 0

	for i, objID := range changed {
		if i >= len(objects) {
			break
		}

		// 定位对象所在的块
		targetBlock, localIdx := mb.locateObject(objID)
		if targetBlock == nil {
			continue // 跳过无效ID
		}

		writeBuffer := targetBlock.GetCurrentWriteBuffer()
		if localIdx >= uint32(len(writeBuffer)) {
			continue // 跳过无效索引
		}

		writeBuffer[localIdx] = objects[i]
		actualWrites++
	}

	if actualWrites > 0 {
		atomic.AddUint64(&mb.stats.totalWrites, uint64(actualWrites))
		atomic.AddUint64(&mb.stats.totalBytes, uint64(actualWrites)*uint64(objectDataSize))
	}
}
