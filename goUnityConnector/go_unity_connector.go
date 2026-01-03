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

type GoUnityConnector struct {
	config ConnectorConfig

	// 核心组件（使用现有内存块，不修改）
	syncManager *extensiveMemoryBlockManager
	opBlock     *OperationMemoryBlock

	// 回调接口（外部注入）
	physicalProvider PhysicalObjectProvider
	commandHandler   OperationCommandHandler

	// 状态管理
	stats    ConnectorStats
	running  bool
	stopChan chan struct{}
	wg       sync.WaitGroup
	mu       sync.RWMutex

	// 性能监控
	frameCounter uint64
	frameTimer   *time.Ticker
}

// NewGoUnityConnector 创建新的连接器
func NewGoUnityConnector(config ConnectorConfig) (*GoUnityConnector, error) {
	// 1. 验证配置
	if config.FrameRate == 0 {
		config.FrameRate = DefaultConnectorConfig.FrameRate
	}
	if config.PollingRate == 0 {
		config.PollingRate = DefaultConnectorConfig.PollingRate
	}

	// 2. 创建物理状态同步管理器
	syncManager, err := NewExtensiveMemoryBlockManager(config.SyncConfig)
	if err != nil {
		return nil, fmt.Errorf("创建物理状态同步管理器失败: %v", err)
	}

	// 3. 创建操作命令内存块（Go端作为消费者，create=false）
	opBlock, err := NewOperationMemoryBlock(config.OpConfig, false)
	if err != nil {
		syncManager.Close()
		return nil, fmt.Errorf("创建操作命令内存块失败: %v", err)
	}

	// 4. 组装连接器
	connector := &GoUnityConnector{
		config:           config,
		syncManager:      syncManager,
		opBlock:          opBlock,
		running:          false,
		stopChan:         make(chan struct{}),
		physicalProvider: nil,
		commandHandler:   nil,
		stats:            ConnectorStats{},
	}

	log.Printf("GoUnityConnector 初始化成功:")
	log.Printf("  物理同步: %s (帧率: %d FPS)", config.SyncConfig.Name, config.FrameRate)
	log.Printf("  命令传输: %s (轮询: %d Hz)", config.OpConfig.Name, config.PollingRate)

	return connector, nil
}

// SetPhysicalObjectProvider 设置物理对象提供者
func (guc *GoUnityConnector) SetPhysicalObjectProvider(provider PhysicalObjectProvider) {
	guc.mu.Lock()
	defer guc.mu.Unlock()
	guc.physicalProvider = provider
}

// SetOperationCommandHandler 设置操作命令处理器
func (guc *GoUnityConnector) SetOperationCommandHandler(handler OperationCommandHandler) {
	guc.mu.Lock()
	defer guc.mu.Unlock()
	guc.commandHandler = handler
}

// NewGoUnityConnector 创建新的连接器
// NewBidirectionalConnector 创建双向连接器

// Start 启动连接器
// Start 启动连接器
func (guc *GoUnityConnector) Start() error {
	guc.mu.Lock()
	defer guc.mu.Unlock()

	if guc.running {
		return fmt.Errorf("连接器已在运行中")
	}

	// 验证回调接口
	if guc.physicalProvider == nil {
		return fmt.Errorf("未设置物理对象提供者，请先调用SetPhysicalObjectProvider")
	}
	if guc.commandHandler == nil {
		return fmt.Errorf("未设置操作命令处理器，请先调用SetOperationCommandHandler")
	}

	guc.running = true
	guc.stopChan = make(chan struct{})

	// 启动物理同步循环
	guc.wg.Add(1)
	go guc.physicalSyncLoop()

	// 启动命令处理循环
	guc.wg.Add(1)
	go guc.commandProcessingLoop()

	// 启动统计日志
	if guc.config.EnableLogging {
		guc.wg.Add(1)
		go guc.logStatsLoop()
	}

	log.Printf("GoUnityConnector 已启动")
	return nil
}

// physicalSyncLoop 物理状态同步循环
func (guc *GoUnityConnector) physicalSyncLoop() {
	defer guc.wg.Done()

	frameDuration := time.Second / time.Duration(guc.config.FrameRate)
	guc.frameTimer = time.NewTicker(frameDuration)
	defer guc.frameTimer.Stop()

	for {
		select {
		case <-guc.stopChan:
			return
		case <-guc.frameTimer.C:
			guc.syncPhysicalFrame()
		}
	}
}

// commandProcessingLoop 命令处理循环
func (guc *GoUnityConnector) commandProcessingLoop() {
	defer guc.wg.Done()

	pollDuration := time.Second / time.Duration(guc.config.PollingRate)
	pollTimer := time.NewTicker(pollDuration)
	defer pollTimer.Stop()

	for {
		select {
		case <-guc.stopChan:
			return
		case <-pollTimer.C:
			guc.processOperationCommands()
		}
	}
}

// Stop 停止连接器
func (guc *GoUnityConnector) Stop() error {
	guc.mu.Lock()
	defer guc.mu.Unlock()

	if !guc.running {
		return nil
	}

	log.Printf("正在停止GoUnityConnector...")

	guc.running = false
	close(guc.stopChan)
	guc.wg.Wait()

	// 关闭内存块
	guc.syncManager.Close()
	guc.opBlock.Close()

	log.Printf("GoUnityConnector 已停止")
	return nil
}

// processPhysicalFrame 处理物理帧
// syncPhysicalFrame 同步物理帧
func (guc *GoUnityConnector) syncPhysicalFrame() {
	startTime := time.Now()

	// 从物理引擎获取对象状态
	objects, changed := guc.physicalProvider.GetObjects()

	if len(objects) > 0 && len(changed) > 0 {
		// 写入共享内存
		guc.syncManager.WriteObjects(objects, changed)

		// 更新头部信息
		guc.syncManager.UpdateHeader(
			guc.frameCounter,
			uint32(len(changed)),
			uint32(len(objects)),
		)

		// 双缓冲切换
		if guc.config.SyncConfig.EnableDoubleBuffer {
			oldIdx, newIdx := guc.syncManager.SwapBuffer()
			if guc.config.EnableLogging {
				log.Printf("双缓冲切换: %d -> %d (变化对象: %d)",
					oldIdx, newIdx, len(changed))
			}
		}

		// 更新统计
		guc.stats.LastSyncTimestamp = time.Now()
		guc.stats.TotalFrames++
	}

	guc.frameCounter++
	guc.stats.AvgFrameTime = time.Since(startTime)
}

// processOperationCommands 处理操作命令
func (guc *GoUnityConnector) processOperationCommands() {
	// 读取所有可用命令
	commands := guc.opBlock.ReadCommands(0)

	if len(commands) > 0 {
		// 传递给OperationHandler
		if err := guc.commandHandler.HandleCommands(commands); err != nil {
			log.Printf("处理命令失败: %v", err)
		}

		// 更新统计
		guc.stats.LastCommandTimestamp = time.Now()
		guc.stats.TotalCommands += uint64(len(commands))

		if guc.config.EnableLogging {
			log.Printf("处理 %d 个操作命令", len(commands))
		}
	}
}

// commandProcessingLoop 命令处理循环
// GetStats 获取统计信息
func (guc *GoUnityConnector) GetStats() ConnectorStats {
	guc.mu.RLock()
	defer guc.mu.RUnlock()
	return guc.stats
}

// GetFrameNumber 获取当前帧号
func (guc *GoUnityConnector) GetFrameNumber() uint64 {
	guc.mu.RLock()
	defer guc.mu.RUnlock()
	return guc.frameCounter
}

// GetSyncManager 获取同步管理器（用于高级操作）
func (guc *GoUnityConnector) GetSyncManager() *extensiveMemoryBlockManager {
	guc.mu.RLock()
	defer guc.mu.RUnlock()
	return guc.syncManager
}

// GetOpBlock 获取操作内存块（用于高级操作）
func (guc *GoUnityConnector) GetOpBlock() *OperationMemoryBlock {
	guc.mu.RLock()
	defer guc.mu.RUnlock()
	return guc.opBlock
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
			stats := guc.GetStats()
			log.Printf("连接器统计 - 帧数: %d, 命令: %d, 平均帧时间: %v",
				stats.TotalFrames, stats.TotalCommands, stats.AvgFrameTime)
		}
	}
}
