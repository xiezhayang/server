// object_sync.go
//go:build windows
// +build windows

package goUnityConnector

import (
	"log"
	"sync"
	"time"
)

// ObjectSyncManager 对象同步管理器
type ObjectSyncManager struct {
	memoryBlock *MemoryBlock
	config      MemoryBlockConfig

	// 对象状态缓存
	objects      []ObjectSyncData // 当前所有对象状态
	objectMap    map[uint32]int   // ObjectID -> objects索引
	changedFlags []bool           // 标记哪些对象发生了变化

	// 帧管理
	currentFrame uint64
	frameTime    time.Duration
	lastUpdate   time.Time

	// 同步控制
	mu           sync.RWMutex
	enableSync   bool
	fullSyncRate int // 每N帧进行一次完整同步
	frameCounter int // 帧计数器

	// 事件回调
	onFrameComplete func(frameNum uint64, changedCount int)
}

// NewObjectSyncManager 创建对象同步管理器
func NewObjectSyncManager(config MemoryBlockConfig) (*ObjectSyncManager, error) {
	// 创建共享内存块
	mb, err := NewMemoryBlock(config, true)
	if err != nil {
		return nil, err
	}

	osm := &ObjectSyncManager{
		memoryBlock:  mb,
		config:       config,
		objects:      make([]ObjectSyncData, config.MaxObjects),
		objectMap:    make(map[uint32]int),
		changedFlags: make([]bool, config.MaxObjects),
		currentFrame: 0,
		frameTime:    time.Millisecond * 16, // 60 FPS
		enableSync:   true,
		fullSyncRate: 10, // 每10帧一次完整同步
		lastUpdate:   time.Now(),
	}

	// 初始化对象数组
	for i := range osm.objects {
		osm.objects[i].ObjectID = uint32(i)
		osm.objects[i].TypeID = 0 // 无效类型，表示空闲槽位
		osm.objectMap[uint32(i)] = i
	}

	return osm, nil
}

// RegisterObject 注册新对象
func (osm *ObjectSyncManager) RegisterObject(objID uint32, typeID ObjectType, x, y float32) bool {
	osm.mu.Lock()
	defer osm.mu.Unlock()

	if objID >= osm.config.MaxObjects {
		log.Printf("错误: 无法注册对象 %d，超出最大容量 %d", objID, osm.config.MaxObjects)
		return false
	}

	// 更新对象状态
	idx := osm.objectMap[objID]
	osm.objects[idx].TypeID = typeID
	osm.objects[idx].PositionX = x
	osm.objects[idx].PositionY = y
	osm.objects[idx].VelocityX = 0
	osm.objects[idx].VelocityY = 0
	osm.objects[idx].Rotation = 0
	osm.objects[idx].StateFlags = StateVisible | StateActive

	// 标记为变化
	osm.changedFlags[idx] = true

	log.Printf("注册对象: ID=%d, 类型=%d, 位置=(%.2f,%.2f)",
		objID, typeID, x, y)

	return true
}

// UpdateObject 更新对象状态
func (osm *ObjectSyncManager) UpdateObject(objID uint32, updateFunc func(*ObjectSyncData)) bool {
	osm.mu.Lock()
	defer osm.mu.Unlock()

	if objID >= osm.config.MaxObjects {
		return false
	}

	idx, exists := osm.objectMap[objID]
	if !exists {
		log.Printf("警告: 尝试更新未注册的对象 %d", objID)
		return false
	}

	// 应用更新函数
	updateFunc(&osm.objects[idx])

	// 标记为变化
	osm.changedFlags[idx] = true

	return true
}

// RemoveObject 移除对象
func (osm *ObjectSyncManager) RemoveObject(objID uint32) bool {
	osm.mu.Lock()
	defer osm.mu.Unlock()

	if objID >= osm.config.MaxObjects {
		return false
	}

	idx, exists := osm.objectMap[objID]
	if !exists {
		return false
	}

	// 标记为不可见/非激活
	osm.objects[idx].TypeID = 0
	osm.objects[idx].StateFlags = 0

	// 标记为变化
	osm.changedFlags[idx] = true

	log.Printf("移除对象: ID=%d", objID)

	return true
}

// BeginFrame 开始新帧
func (osm *ObjectSyncManager) BeginFrame() {
	osm.mu.Lock()
	osm.lastUpdate = time.Now()
	osm.currentFrame++

	// 重置变化标志
	for i := range osm.changedFlags {
		osm.changedFlags[i] = false
	}
}

// EndFrame 结束帧，执行同步
func (osm *ObjectSyncManager) EndFrame() {
	osm.mu.Lock()
	defer osm.mu.Unlock()

	if !osm.enableSync {
		return
	}

	// 计算帧时间
	osm.frameTime = time.Since(osm.lastUpdate)

	// 收集变化的对象
	changedObjects := make([]ObjectSyncData, 0, osm.config.MaxObjects)
	changedIDs := make([]uint32, 0, osm.config.MaxObjects)
	totalCount := 0

	for i := range osm.objects {
		if osm.objects[i].TypeID != 0 { // 有效对象
			totalCount++
		}
		if osm.changedFlags[i] && osm.objects[i].TypeID != 0 {
			changedObjects = append(changedObjects, osm.objects[i])
			changedIDs = append(changedIDs, uint32(i))
		}
	}

	// 确定同步标志
	flags := uint32(0)
	osm.frameCounter++

	// 是否进行完整同步
	if osm.frameCounter >= osm.fullSyncRate {
		flags |= 0x1 // 完整同步标志
		osm.frameCounter = 0

		// 完整同步时，写入所有有效对象
		changedObjects = make([]ObjectSyncData, 0, totalCount)
		changedIDs = make([]uint32, 0, totalCount)

		for i := range osm.objects {
			if osm.objects[i].TypeID != 0 {
				changedObjects = append(changedObjects, osm.objects[i])
				changedIDs = append(changedIDs, uint32(i))
			}
		}
	}

	// 写入共享内存
	if len(changedObjects) > 0 {
		osm.memoryBlock.WriteObjects(changedObjects, changedIDs)

		// 更新头部
		osm.memoryBlock.UpdateHeader(
			osm.currentFrame,
			uint32(len(changedObjects)),
			uint32(totalCount),
			flags,
		)

		// 如果是双缓冲，切换缓冲区
		if osm.config.EnableDoubleBuffer {
			oldIdx, newIdx := osm.memoryBlock.SwapBuffer()
			log.Printf("双缓冲切换: %d -> %d (变化对象: %d)", oldIdx, newIdx, len(changedObjects))
		}

		// 触发回调
		if osm.onFrameComplete != nil {
			osm.onFrameComplete(osm.currentFrame, len(changedObjects))
		}

		// 调试信息（每60帧输出一次）
		if osm.currentFrame%60 == 0 {
			log.Printf("同步完成: 帧=%d, 变化=%d, 总计=%d, 时间=%v",
				osm.currentFrame, len(changedObjects), totalCount, osm.frameTime)
		}
	}
}

// GetSnapshot 获取当前帧的快照
func (osm *ObjectSyncManager) GetSnapshot() *Snapshot {
	osm.mu.RLock()
	defer osm.mu.RUnlock()

	snapshot := &Snapshot{
		FrameNumber: osm.currentFrame,
		Timestamp:   time.Now(),
		Changed:     make([]uint32, 0),
	}

	// 收集所有有效对象
	for i := range osm.objects {
		if osm.objects[i].TypeID != 0 {
			snapshot.Objects = append(snapshot.Objects, osm.objects[i])
		}
		if osm.changedFlags[i] && osm.objects[i].TypeID != 0 {
			snapshot.Changed = append(snapshot.Changed, uint32(i))
		}
	}

	return snapshot
}

// SetFrameCompleteCallback 设置帧完成回调
func (osm *ObjectSyncManager) SetFrameCompleteCallback(callback func(frameNum uint64, changedCount int)) {
	osm.mu.Lock()
	defer osm.mu.Unlock()
	osm.onFrameComplete = callback
}

// EnableSync 启用/禁用同步
func (osm *ObjectSyncManager) EnableSync(enable bool) {
	osm.mu.Lock()
	defer osm.mu.Unlock()
	osm.enableSync = enable
	log.Printf("同步 %s", map[bool]string{true: "启用", false: "禁用"}[enable])
}

// Close 关闭同步管理器
func (osm *ObjectSyncManager) Close() {
	osm.mu.Lock()
	defer osm.mu.Unlock()

	osm.enableSync = false
	if osm.memoryBlock != nil {
		osm.memoryBlock.Close()
		osm.memoryBlock = nil
	}

	log.Printf("对象同步管理器已关闭")
}
