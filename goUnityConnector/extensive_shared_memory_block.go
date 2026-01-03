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
	config        extensiveMemoryBlockConfig
	header        *extensiveHeader // 改为指针，指向共享内存中的头部
	headerHandle  syscall.Handle   // 头部共享内存的文件句柄
	headerPtr     uintptr          // 头部共享内存的映射指针
	blocks        []*extensiveMemoryBlock
	objectLocks   []sync.Mutex
	lock          sync.Mutex
	nextExtension uint64
	freeList      []uint64
	nextID        uint64 // 下一个新ID（从未使用过的）
	// 统计信息
	stats struct {
		totalWrites    uint64
		totalBytes     uint64
		extensions     uint64 // 扩展次数
		maxObjectsEver uint32 // 历史最大对象数
	}
}

type extensiveMemoryBlock struct {
	hMapFile     syscall.Handle
	dataBuffers  [2][]ObjectSyncData
	blockID      uint64 // 块链唯一标识
	objectsCount uint32
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
	headerName := fmt.Sprintf("%s_header", config.Name)
	headerNamePtr, err := syscall.UTF16PtrFromString(headerName)
	if err != nil {
		return nil, fmt.Errorf("头部名称转换失败: %v", err)
	}

	// 计算头部内存大小（对齐到页面边界，Windows通常为4KB）
	headerSize := extensiveHeaderSize
	if headerSize%4096 != 0 {
		headerSize = ((headerSize / 4096) + 1) * 4096
	}

	// 创建头部文件映射
	headerHandle, _, err := procCreateFileMappingW.Call(
		uintptr(INVALID_HANDLE_VALUE), // 使用系统页面文件
		0,                             // 安全属性
		uintptr(PAGE_READWRITE),       // 保护模式
		0,                             // 高位大小
		uintptr(headerSize),           // 低位大小
		uintptr(unsafe.Pointer(headerNamePtr)),
	)

	if headerHandle == 0 {
		return nil, fmt.Errorf("创建头部共享内存失败: %v", err)
	}

	// 映射头部共享内存视图
	headerPtr, _, err := procMapViewOfFile.Call(
		uintptr(headerHandle),
		uintptr(FILE_MAP_ALL_ACCESS),
		0,
		0,
		uintptr(headerSize),
	)

	if headerPtr == 0 {
		procCloseHandle.Call(uintptr(headerHandle))
		return nil, fmt.Errorf("映射头部共享内存失败: %v", err)
	}
	header := (*extensiveHeader)(unsafe.Pointer(headerPtr))
	header.Version = 1
	header.MaxObjects = config.MaxObjects
	header.FrameNumber = 0
	header.Timestamp = 0
	header.ChangedCount = 0
	header.TotalObjects = 0
	if config.EnableDoubleBuffer {
		header.BackBuffer = 1
		header.FrontBuffer = 0
	} else {
		header.BackBuffer = 0
		header.FrontBuffer = 0 // 单缓冲时 FrontBuffer 也应设为 0
	}
	embm := &extensiveMemoryBlockManager{
		config:        config,
		header:        header,
		headerHandle:  syscall.Handle(headerHandle),
		headerPtr:     headerPtr,
		blocks:        make([]*extensiveMemoryBlock, 0),
		objectLocks:   make([]sync.Mutex, config.MaxObjects),
		lock:          sync.Mutex{},
		nextExtension: uint64(config.ExtendThreshold * float64(config.MaxObjects)),
		freeList:      make([]uint64, 0),
		nextID:        1,
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
	if config.EnableAutoExtend {
		// 初始只有一个块，总容量 = MaxObjects
		totalCapacity := uint64(config.MaxObjects)
		embm.nextExtension = uint64(float64(totalCapacity) * config.ExtendThreshold)
	}

	log.Printf("共享内存管理器 %s 已创建 (头部大小: %d 字节, 对象容量: %d, 双缓冲: %v)",
		config.Name, headerSize, config.MaxObjects, config.EnableDoubleBuffer)
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
		blockID = uint64(len(embm.blocks))
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
			data: ptr + bufferDataSize/2,
			len:  int(embm.config.MaxObjects + 1),
			cap:  int(embm.config.MaxObjects + 1),
		}
		mb.dataBuffers[1] = *(*[]ObjectSyncData)(unsafe.Pointer(sliceHeader1))
	} else {
		// 单缓冲：缓冲区1设为nil，避免混淆
		mb.dataBuffers[1] = nil
	}
	embm.blocks = append(embm.blocks, mb)
	embm.nextExtension += uint64(float64(embm.config.MaxObjects) * embm.config.ExtendThreshold)
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
	idx := atomic.LoadUint32(&embm.header.BackBuffer)
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

	for {
		currentFront := atomic.LoadUint32(&embm.header.FrontBuffer)
		currentBack := atomic.LoadUint32(&embm.header.BackBuffer)

		// 如果Front和Back相同，不需要交换（理论上不应该发生）
		if currentFront == currentBack {
			return currentFront, currentBack
		}

		// 尝试原子交换
		if atomic.CompareAndSwapUint32(&embm.header.FrontBuffer, currentFront, currentBack) {
			// 第一个成功，立即更新第二个
			atomic.StoreUint32(&embm.header.BackBuffer, currentFront)
			return currentFront, currentBack
		}
		// CAS失败，重试
	}
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

// Close 关闭共享内存管理器，释放所有资源
func (embm *extensiveMemoryBlockManager) Close() {
	// 使用锁确保线程安全，防止并发关闭
	embm.lock.Lock()
	defer embm.lock.Unlock()

	// 1. 关闭头部共享内存
	if embm.headerPtr != 0 {
		procUnmapViewOfFile.Call(embm.headerPtr)
		embm.headerPtr = 0
		embm.header = nil // 防止野指针
	}

	if embm.headerHandle != 0 {
		procCloseHandle.Call(uintptr(embm.headerHandle))
		embm.headerHandle = 0
	}

	// 2. 关闭所有数据块
	for _, block := range embm.blocks {
		if block == nil {
			continue
		}

		// 2.1 从 dataBuffers[0] 获取映射指针并取消映射
		// 注意：需要检查切片是否有效，长度是否大于0
		if block.dataBuffers[0] != nil && len(block.dataBuffers[0]) > 0 {
			// 获取切片底层数组的起始地址
			// 这是 MapViewOfFile 返回的指针，也就是整个映射视图的基地址
			ptr := uintptr(unsafe.Pointer(&block.dataBuffers[0][0]))
			if ptr != 0 {
				procUnmapViewOfFile.Call(ptr)
			}
		}

		// 2.2 关闭文件映射句柄
		if block.hMapFile != 0 {
			procCloseHandle.Call(uintptr(block.hMapFile))
			block.hMapFile = 0
		}

		// 2.3 清空数据缓冲区引用（避免意外访问）
		block.dataBuffers[0] = nil
		block.dataBuffers[1] = nil

		log.Printf("数据块 %s_%d 已关闭", embm.config.Name, block.blockID)
	}

	// 3. 清空管理器状态
	embm.blocks = nil
	embm.freeList = nil
	embm.nextID = 1
	embm.nextExtension = 0

	// 4. 重置统计信息
	atomic.StoreUint64(&embm.stats.totalWrites, 0)
	atomic.StoreUint64(&embm.stats.totalBytes, 0)
	atomic.StoreUint64(&embm.stats.extensions, 0)
	atomic.StoreUint32(&embm.stats.maxObjectsEver, 0)

	log.Printf("共享内存管理器 %s 已完全关闭", embm.config.Name)
}

// GetBufferInfo 获取缓冲区信息（调试用）
func (embm *extensiveMemoryBlockManager) GetBufferInfo() (frontIdx, backIdx uint32) {
	return atomic.LoadUint32(&embm.header.FrontBuffer),
		atomic.LoadUint32(&embm.header.BackBuffer)
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

// allocateObjectID 分配一个新的或重用的ObjectID
func (embm *extensiveMemoryBlockManager) allocateObjectID() (uint64, bool) {
	embm.lock.Lock()
	defer embm.lock.Unlock()

	var objID uint64

	// 1. 优先从空闲栈弹出（重用）
	if len(embm.freeList) > 0 {
		// 弹出最后一个元素（O(1)）
		lastIdx := len(embm.freeList) - 1
		objID = embm.freeList[lastIdx]
		embm.freeList = embm.freeList[:lastIdx]

	} else {
		// 2. 检查是否需要扩展新块

		if embm.nextID >= embm.nextExtension {
			if !embm.config.EnableAutoExtend {
				return 0, false // 容量已满且不允许扩展
			}

			// 创建新块
			if err := embm.NewExtensiveMemoryBlock(0); err != nil {
				log.Printf("扩展块失败: %v", err)

				return 0, false
			}
			newObjectlocks := make([]sync.Mutex, embm.config.MaxObjects)
			embm.objectLocks = append(embm.objectLocks, newObjectlocks...)
			// 更新容量
			embm.nextExtension += uint64(float64(embm.config.MaxObjects) * embm.config.ExtendThreshold)
		}

		// 3. 分配全新ID
		objID = embm.nextID
		embm.nextID++
	}

	return objID, true
}

func (embm *extensiveMemoryBlockManager) locateObject(objID uint64) (targetBlock *extensiveMemoryBlock, localIdx uint64) {
	if embm.header.MaxObjects == 0 {
		return nil, 0
	}
	localIdx = objID % uint64(embm.header.MaxObjects)
	blockIdx := objID / uint64(embm.header.MaxObjects)
	if blockIdx >= uint64(len(embm.blocks)) {
		return nil, 0
	}
	return embm.blocks[blockIdx], localIdx
}

// WriteObject 写入单个对象数据（支持扩展块）
func (embm *extensiveMemoryBlockManager) WriteObject(objID uint64, data *ObjectSyncData) bool {
	// 定位对象所在的块
	if objID == 0 {
		objid, success := embm.allocateObjectID()
		if !success {
			log.Printf("错误: 分配ObjectID失败")
			return false

		}
		objID = objid
	}
	embm.objectLocks[objID].Lock()
	defer embm.objectLocks[objID].Unlock()
	targetBlock, localIdx := embm.locateObject(objID)
	if targetBlock == nil {
		log.Printf("错误: ObjectID %d 超出所有块范围", objID)
		return false
	}

	// 获取目标块的当前写入缓冲区
	writeBuffer := embm.GetCurrentWriteBuffer()

	if localIdx >= uint64(len(targetBlock.dataBuffers[writeBuffer])) {
		log.Printf("错误: 局部索引 %d 超出缓冲区大小 %d", localIdx, len(targetBlock.dataBuffers[writeBuffer]))
		return false
	}

	targetBlock.dataBuffers[writeBuffer][localIdx] = *data

	// 更新统计
	atomic.AddUint64(&embm.stats.totalWrites, 1)
	atomic.AddUint64(&embm.stats.totalBytes, uint64(objectDataSize))

	return true
}

// WriteObjects 批量写入对象数据（支持扩展块）
func (embm *extensiveMemoryBlockManager) WriteObjects(objects []ObjectSyncData, changed []uint64) {
	if len(objects) == 0 || len(changed) == 0 {
		return
	}

	actualWrites := 0

	for i, objID := range changed {
		if i >= len(objects) {
			break
		}

		if !embm.WriteObject(objID, &objects[i]) {
			log.Printf("错误: 写入对象 %d 失败", objID)
			continue
		}
		actualWrites++
	}
}

func (embm *extensiveMemoryBlockManager) RemoveObject(objID uint64) bool {
	// 1. 验证输入
	if objID == 0 {
		return false // ID 0 是保留的，无效
	}

	embm.objectLocks[objID].Lock()
	defer embm.objectLocks[objID].Unlock()

	// 2. 定位对象
	targetBlock, localIdx := embm.locateObject(objID)
	if targetBlock == nil {
		return false
	}
	// 清理缓冲区0（始终存在）
	if localIdx < uint64(len(targetBlock.dataBuffers[0])) {
		targetBlock.dataBuffers[0][localIdx] = ObjectSyncData{}
	}

	// 如果启用双缓冲，同时清理缓冲区1
	if embm.config.EnableDoubleBuffer && targetBlock.dataBuffers[1] != nil {
		if localIdx < uint64(len(targetBlock.dataBuffers[1])) {
			targetBlock.dataBuffers[1][localIdx] = ObjectSyncData{}
		}
	}

	// 4. 回收ID到空闲列表（LIFO，O(1)操作）
	embm.freeList = append(embm.freeList, objID)

	// 5. 安全递减TotalObjects计数（避免减到0以下）
	for {
		current := atomic.LoadUint32(&embm.header.TotalObjects)
		if current == 0 {
			// 对象计数已为0，无法递减（可能是重复删除或状态不一致）
			break
		}
		newVal := current - 1
		if atomic.CompareAndSwapUint32(&embm.header.TotalObjects, current, newVal) {
			break
		}
	}

	return true
}
