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
	config BidirectionalConnectorConfig

	// 内存块
	syncBlock *extensiveMemoryBlock // 物理状态同步
	opBlock   *OperationMemoryBlock // 操作命令传输

	// 状态管理
	stats         ConnectorStats
	eventHandlers []EventHandler
	running       bool
	stopChan      chan struct{}
	wg            sync.WaitGroup
	mu            sync.RWMutex

	// 帧计数器
	frameCounter uint64
	frameTimer   *time.Ticker
}

// ConnectorConfig 连接器配置
// BidirectionalConnectorConfig 双向连接器配置
type BidirectionalConnectorConfig struct {
	// Go->Unity 物理状态同步配置
	GoToUnity struct {
		BlockName          string
		MaxObjects         uint32
		FrameRate          int // 目标帧率（如60）
		EnableDoubleBuffer bool
		FullSyncInterval   int // 完整同步间隔（帧数）
	}

	// Unity->Go 操作命令配置
	UnityToGo struct {
		BlockName   string
		MaxCommands uint32 // 命令队列容量（如256）
		PollingRate int    // 轮询频率（如120Hz）
		EnableLog   bool   // 启用操作日志
	}

	// 通用配置
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
// NewBidirectionalConnector 创建双向连接器
func NewBidirectionalConnector(config BidirectionalConnectorConfig) (*GoUnityConnector, error) {
	// 创建对象同步内存块
	syncConfig := extensiveMemoryBlockConfig{
		Name:               config.GoToUnity.BlockName,
		MaxObjects:         config.GoToUnity.MaxObjects,
		EnableDoubleBuffer: config.GoToUnity.EnableDoubleBuffer,
	}

	syncBlock, err := NewExtensiveMemoryBlock(syncConfig, true)
	if err != nil {
		return nil, fmt.Errorf("创建对象同步内存块失败: %v", err)
	}

	// 创建操作命令内存块
	opConfig := OperationMemoryBlockConfig{
		Name:      config.UnityToGo.BlockName,
		Capacity:  config.UnityToGo.MaxCommands,
		EnableLog: config.UnityToGo.EnableLog,
	}

	opBlock, err := NewOperationMemoryBlock(opConfig, true)
	if err != nil {
		syncBlock.Close()
		return nil, fmt.Errorf("创建操作命令内存块失败: %v", err)
	}

	connector := &GoUnityConnector{
		config:        config,
		syncBlock:     syncBlock,
		opBlock:       opBlock,
		stats:         ConnectorStats{},
		running:       false,
		stopChan:      make(chan struct{}),
		eventHandlers: make([]EventHandler, 0),
	}

	log.Printf("双向连接器初始化完成:")
	log.Printf("  同步块: %s (容量: %d 对象, 双缓冲: %v)",
		config.GoToUnity.BlockName, config.GoToUnity.MaxObjects,
		config.GoToUnity.EnableDoubleBuffer)
	log.Printf("  操作块: %s (容量: %d 命令, 轮询: %dHz)",
		config.UnityToGo.BlockName, config.UnityToGo.MaxCommands,
		config.UnityToGo.PollingRate)

	return connector, nil
}

// Start 启动连接器
// Start 启动连接器
func (guc *GoUnityConnector) Start() error {
	guc.mu.Lock()
	defer guc.mu.Unlock()

	if guc.running {
		return fmt.Errorf("连接器已在运行中")
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

	log.Printf("双向连接器已启动")
	log.Printf("  物理同步: %d FPS", guc.config.GoToUnity.FrameRate)
	log.Printf("  命令处理: %d Hz", guc.config.UnityToGo.PollingRate)

	return nil
}

// Stop 停止连接器
func (guc *GoUnityConnector) Stop() error {
	guc.mu.Lock()
	defer guc.mu.Unlock()

	if !guc.running {
		return nil
	}

	log.Printf("正在停止双向连接器...")

	guc.running = false
	close(guc.stopChan)
	guc.wg.Wait()

	// 关闭内存块
	guc.syncBlock.Close()
	guc.opBlock.Close()

	log.Printf("双向连接器已停止")
	return nil
}

// Stop 停止连接器
// physicalSyncLoop 物理状态同步循环
func (guc *GoUnityConnector) physicalSyncLoop() {
	defer guc.wg.Done()

	frameDuration := time.Second / time.Duration(guc.config.GoToUnity.FrameRate)
	guc.frameTimer = time.NewTicker(frameDuration)
	defer guc.frameTimer.Stop()

	for {
		select {
		case <-guc.stopChan:
			return
		case <-guc.frameTimer.C:
			guc.processPhysicalFrame()
		}
	}
}

// processPhysicalFrame 处理物理帧
func (guc *GoUnityConnector) processPhysicalFrame() {
	startTime := time.Now()

	// 从物理引擎获取对象状态（由外部实现）
	objects, changed := guc.getPhysicalObjects()

	// 写入共享内存
	if len(objects) > 0 && len(changed) > 0 {
		guc.syncBlock.WriteObjects(objects, changed)

		// 更新头部信息
		guc.syncBlock.UpdateHeader(
			guc.frameCounter,
			uint32(len(changed)),
			uint32(len(objects)),
			0, // 标志位
		)

		// 双缓冲切换
		if guc.config.GoToUnity.EnableDoubleBuffer {
			oldIdx, newIdx := guc.syncBlock.SwapBuffer()
			if guc.config.EnableLogging {
				log.Printf("双缓冲切换: %d -> %d (变化对象: %d)",
					oldIdx, newIdx, len(changed))
			}
		}

		// 通知事件处理器
		guc.notifyFrameSyncComplete(guc.frameCounter, len(changed))
	}

	guc.frameCounter++
	guc.stats.LastFrameTime = time.Since(startTime)
	guc.stats.TotalFrames++
}

// commandProcessingLoop 命令处理循环
func (guc *GoUnityConnector) commandProcessingLoop() {
	defer guc.wg.Done()

	pollDuration := time.Second / time.Duration(guc.config.UnityToGo.PollingRate)
	pollTimer := time.NewTicker(pollDuration)
	defer pollTimer.Stop()

	for {
		select {
		case <-guc.stopChan:
			return
		case <-pollTimer.C:
			guc.processCommands()
		}
	}
}

// processCommands 处理接收到的命令
func (guc *GoUnityConnector) processCommands() {
	// 读取所有可用命令
	commands := guc.opBlock.ReadCommands(0)

	if len(commands) > 0 {
		// 统计更新
		guc.stats.TotalObjects += uint64(len(commands))

		// 将命令传递给operation handler（由外部实现）
		guc.handleOperationCommands(commands)

		if guc.config.EnableLogging && len(commands) > 0 {
			log.Printf("处理 %d 个操作命令", len(commands))
		}
	}
}

// WriteObject 写入单个对象状态（供物理引擎调用）
func (guc *GoUnityConnector) WriteObject(objID uint32, data *ObjectSyncData) bool {
	return guc.syncBlock.WriteObject(objID, data)
}

// WriteObjects 批量写入对象状态
func (guc *GoUnityConnector) WriteObjects(objects []ObjectSyncData, changed []uint32) {
	guc.syncBlock.WriteObjects(objects, changed)
}

// ReadCommands 读取操作命令（供operation handler调用）
func (guc *GoUnityConnector) ReadCommands(maxCount int) []OperationCommand {
	return guc.opBlock.ReadCommands(maxCount)
}

// WriteCommand 写入操作命令（供Unity端调用，通过C#包装器）
func (guc *GoUnityConnector) WriteCommand(cmd *OperationCommand) bool {
	return guc.opBlock.WriteCommand(cmd)
}

// SwapBuffer 手动触发双缓冲切换
func (guc *GoUnityConnector) SwapBuffer() (oldIndex, newIndex int32) {
	return guc.syncBlock.SwapBuffer()
}

// GetSyncStats 获取同步统计信息
func (guc *GoUnityConnector) GetSyncStats() (writes, bytes uint64) {
	return guc.syncBlock.GetStats()
}

// GetOpStats 获取操作统计信息
func (guc *GoUnityConnector) GetOpStats() (writes, reads, overflow uint64) {
	return guc.opBlock.GetStats()
}

// GetAvailableCommandSpace 获取命令缓冲区可用空间
func (guc *GoUnityConnector) GetAvailableCommandSpace() int {
	return guc.opBlock.GetAvailableSpace()
}

// IsCommandBufferFull 检查命令缓冲区是否已满
func (guc *GoUnityConnector) IsCommandBufferFull() bool {
	return guc.opBlock.IsFull()
}

// IsCommandBufferEmpty 检查命令缓冲区是否为空
func (guc *GoUnityConnector) IsCommandBufferEmpty() bool {
	return guc.opBlock.IsEmpty()
}
