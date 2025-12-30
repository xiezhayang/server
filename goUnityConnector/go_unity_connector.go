// go_unity_connector.go
//go:build windows
// +build windows

package goUnityConnector

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// GoUnityConnector 主连接器接口
type GoUnityConnector struct {
	config        ConnectorConfig
	syncManager   *ObjectSyncManager
	stats         ConnectorStats
	eventHandlers []EventHandler
	running       bool
	stopChan      chan struct{}
	wg            sync.WaitGroup
	mu            sync.RWMutex
}

// ConnectorConfig 连接器配置
type ConnectorConfig struct {
	// 同步配置
	SyncBlockName      string
	MaxObjects         uint32
	FrameRate          int // 目标帧率
	EnableDoubleBuffer bool

	// 性能配置
	FullSyncInterval int // 完整同步间隔（帧数）
	ChangeThreshold  int // 变化阈值，超过此值触发完整同步

	// 调试配置
	EnableLogging bool
	LogInterval   time.Duration
}

// EventHandler 事件处理器接口
type EventHandler interface {
	OnObjectRegistered(objID uint32, typeID ObjectType)
	OnObjectUpdated(objID uint32, data *ObjectSyncData)
	OnObjectRemoved(objID uint32)
	OnFrameSyncComplete(frameNum uint64, changedCount int)
}

// DefaultConfig 默认配置
var DefaultConfig = ConnectorConfig{
	SyncBlockName:      "UnityGoObjectSync",
	MaxObjects:         256,
	FrameRate:          60,
	EnableDoubleBuffer: true,
	FullSyncInterval:   10,
	ChangeThreshold:    50, // 50%变化时触发完整同步
	EnableLogging:      true,
	LogInterval:        time.Second * 5,
}

// NewGoUnityConnector 创建新的连接器
func NewGoUnityConnector(config ConnectorConfig) (*GoUnityConnector, error) {
	if config.SyncBlockName == "" {
		config.SyncBlockName = DefaultConfig.SyncBlockName
	}
	if config.MaxObjects == 0 {
		config.MaxObjects = DefaultConfig.MaxObjects
	}
	if config.FrameRate == 0 {
		config.FrameRate = DefaultConfig.FrameRate
	}

	// 创建内存块配置
	memConfig := MemoryBlockConfig{
		Name:               config.SyncBlockName,
		MaxObjects:         config.MaxObjects,
		EnableDoubleBuffer: config.EnableDoubleBuffer,
		BufferCount:        2,
	}

	// 创建对象同步管理器
	syncManager, err := NewObjectSyncManager(memConfig)
	if err != nil {
		return nil, fmt.Errorf("创建同步管理器失败: %v", err)
	}

	connector := &GoUnityConnector{
		config:      config,
		syncManager: syncManager,
		stats:       ConnectorStats{},
		running:     false,
		stopChan:    make(chan struct{}),
	}

	// 设置帧完成回调
	syncManager.SetFrameCompleteCallback(func(frameNum uint64, changedCount int) {
		connector.updateStats(frameNum, changedCount)
		connector.notifyFrameSyncComplete(frameNum, changedCount)
	})

	log.Printf("GoUnityConnector 初始化完成: 内存块=%s, 最大对象=%d",
		config.SyncBlockName, config.MaxObjects)

	return connector, nil
}

// Start 启动连接器
func (guc *GoUnityConnector) Start() error {
	guc.mu.Lock()
	defer guc.mu.Unlock()

	if guc.running {
		return fmt.Errorf("连接器已在运行中")
	}

	guc.running = true
	guc.stopChan = make(chan struct{})

	// 启动同步循环
	guc.wg.Add(1)
	go guc.syncLoop()

	// 启动统计日志（如果启用）
	if guc.config.EnableLogging {
		guc.wg.Add(1)
		go guc.logStatsLoop()
	}

	log.Printf("GoUnityConnector 已启动，目标帧率: %d FPS", guc.config.FrameRate)

	return nil
}

// Stop 停止连接器
func (guc *GoUnityConnector) Stop() error {
	guc.mu.Lock()
	defer guc.mu.Unlock()

	if !guc.running {
		return nil
	}

	log.Printf("正在停止 GoUnityConnector...")

	guc.running = false
	close(guc.stopChan)
	guc.wg.Wait()

	// 关闭同步管理器
	if guc.syncManager != nil {
		guc.syncManager.Close()
	}

	log.Printf("GoUnityConnector 已停止")

	return nil
}

// syncLoop 同步循环
func (guc *GoUnityConnector) syncLoop() {
	defer guc.wg.Done()

	frameDuration := time.Second / time.Duration(guc.config.FrameRate)
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()

	for {
		select {
		case <-guc.stopChan:
			return
		case <-ticker.C:
			guc.processFrame()
		}
	}
}

// processFrame 处理单帧
func (guc *GoUnityConnector) processFrame() {
	startTime := time.Now()

	// 开始新帧
	guc.syncManager.BeginFrame()

	// TODO: 这里可以插入游戏逻辑更新对象状态的代码
	// 例如：guc.updateGameObjects()

	// 结束帧，执行同步
	guc.syncManager.EndFrame()

	// 更新统计
	guc.stats.LastFrameTime = time.Since(startTime)
	guc.stats.TotalFrames++
}

// RegisterObject 注册新对象（对外接口）
func (guc *GoUnityConnector) RegisterObject(objID uint32, typeID ObjectType, x, y float32) bool {
	success := guc.syncManager.RegisterObject(objID, typeID, x, y)
	if success {
		guc.notifyObjectRegistered(objID, typeID)
	}
	return success
}

// UpdateObject 更新对象状态（对外接口）
func (guc *GoUnityConnector) UpdateObject(objID uint32, data *ObjectSyncData) bool {
	return guc.syncManager.UpdateObject(objID, func(obj *ObjectSyncData) {
		*obj = *data
	})
}

// RemoveObject 移除对象（对外接口）
func (guc *GoUnityConnector) RemoveObject(objID uint32) bool {
	success := guc.syncManager.RemoveObject(objID)
	if success {
		guc.notifyObjectRemoved(objID)
	}
	return success
}

// GetSnapshot 获取当前快照
func (guc *GoUnityConnector) GetSnapshot() *Snapshot {
	return guc.syncManager.GetSnapshot()
}

// AddEventHandler 添加事件处理器
func (guc *GoUnityConnector) AddEventHandler(handler EventHandler) {
	guc.mu.Lock()
	defer guc.mu.Unlock()
	guc.eventHandlers = append(guc.eventHandlers, handler)
}

// updateStats 更新统计信息
func (guc *GoUnityConnector) updateStats(frameNum uint64, changedCount int) {
	guc.mu.Lock()
	defer guc.mu.Unlock()

	guc.stats.TotalObjects += uint64(changedCount)

	// 计算平均变化率（滑动窗口）
	if guc.stats.TotalFrames > 0 {
		totalFrames := float64(guc.stats.TotalFrames)
		totalChanges := float64(guc.stats.TotalObjects)
		guc.stats.AvgChangesPerFrame = totalChanges / totalFrames

		// 估算带宽（假设每个对象28字节）
		guc.stats.AvgBandwidth = guc.stats.AvgChangesPerFrame * 28.0
	}
}

// logStatsLoop 统计日志循环
func (guc *GoUnityConnector) logStatsLoop() {
	defer guc.wg.Done()

	ticker := time.NewTicker(guc.config.LogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-guc.stopChan:
			return
		case <-ticker.C:
			guc.logStats()
		}
	}
}

// logStats 输出统计信息
func (guc *GoUnityConnector) logStats() {
	guc.mu.RLock()
	stats := guc.stats
	running := guc.running
	guc.mu.RUnlock()

	if running {
		log.Printf("同步统计: 帧=%d, 平均变化=%.1f/帧, 带宽≈%.1f B/帧, 帧时间=%v",
			stats.TotalFrames, stats.AvgChangesPerFrame,
			stats.AvgBandwidth, stats.LastFrameTime)
	}
}

// 事件通知方法
func (guc *GoUnityConnector) notifyObjectRegistered(objID uint32, typeID ObjectType) {
	guc.mu.RLock()
	handlers := guc.eventHandlers
	guc.mu.RUnlock()

	for _, handler := range handlers {
		handler.OnObjectRegistered(objID, typeID)
	}
}

func (guc *GoUnityConnector) notifyObjectUpdated(objID uint32, data *ObjectSyncData) {
	guc.mu.RLock()
	handlers := guc.eventHandlers
	guc.mu.RUnlock()

	for _, handler := range handlers {
		handler.OnObjectUpdated(objID, data)
	}
}

func (guc *GoUnityConnector) notifyObjectRemoved(objID uint32) {
	guc.mu.RLock()
	handlers := guc.eventHandlers
	guc.mu.RUnlock()

	for _, handler := range handlers {
		handler.OnObjectRemoved(objID)
	}
}

func (guc *GoUnityConnector) notifyFrameSyncComplete(frameNum uint64, changedCount int) {
	guc.mu.RLock()
	handlers := guc.eventHandlers
	guc.mu.RUnlock()

	for _, handler := range handlers {
		handler.OnFrameSyncComplete(frameNum, changedCount)
	}
}

// GetStats 获取统计信息
func (guc *GoUnityConnector) GetStats() ConnectorStats {
	guc.mu.RLock()
	defer guc.mu.RUnlock()
	return guc.stats
}

// IsRunning 检查是否在运行
func (guc *GoUnityConnector) IsRunning() bool {
	guc.mu.RLock()
	defer guc.mu.RUnlock()
	return guc.running
}
