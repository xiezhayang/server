// types.go
//go:build windows
// +build windows

package goUnityConnector

import (
	"time"
)

// ObjectType 对象类型枚举
type ObjectType uint32

const (
	ObjPlayer  ObjectType = 1
	ObjEgg     ObjectType = 2
	ObjCoin    ObjectType = 3
	ObjMonster ObjectType = 4
	// ... 可扩展更多类型
)

// ObjectStateFlags 对象状态位掩码
type ObjectStateFlags uint32

const (
	StateVisible   ObjectStateFlags = 1 << 0 // 是否可见
	StateActive    ObjectStateFlags = 1 << 1 // 是否激活
	StateGrounded  ObjectStateFlags = 1 << 2 // 是否在地面
	StateJumping   ObjectStateFlags = 1 << 3 // 是否跳跃
	StateAttacking ObjectStateFlags = 1 << 4 // 是否攻击
	StateDamaged   ObjectStateFlags = 1 << 5 // 是否受伤
	// ... 预留更多状态位
)

// AnimationState 动画状态（使用位掩码的一部分）
type AnimationState uint8

// ObjectSyncData 对象同步数据（共享内存中的布局）
type ObjectSyncData struct {
	ObjectID   uint32           // 对象唯一标识（由Go生成）
	TypeID     ObjectType       // 对象类型（决定Unity使用哪个预制体）
	PositionX  float32          // 世界坐标X
	PositionY  float32          // 世界坐标Y
	VelocityX  float32          // X方向速度
	VelocityY  float32          // Y方向速度
	Rotation   float32          // 旋转角度（弧度）
	StateFlags ObjectStateFlags // 状态位掩码
	Animation  AnimationState   // 动画状态
}

// SyncHeader 同步内存块头部
type extensiveHeader struct {
	Version      uint32 // 协议版本
	MaxObjects   uint32 // 最大对象容量（固定）
	FrameNumber  uint64 // 全局物理帧号
	Timestamp    uint64 // 最后更新时间戳（纳秒）
	ChangedCount uint32 // 本帧变化对象数量
	TotalObjects uint32 // 总有效对象数量
	FrontBuffer  uint32 // Unity读取的缓冲区索引（0或1）
	BackBuffer   uint32 // Go写入的缓冲区索引（0或1）
}

// MemoryBlockConfig 内存块配置
type extensiveMemoryBlockConfig struct {
	Name               string // 共享内存名称
	MaxObjects         uint32 // 初始最大对象数量
	EnableDoubleBuffer bool   // 是否启用双缓冲

	// 扩展策略
	EnableAutoExtend bool    // 是否启用自动扩展
	ExtendThreshold  float64 // 扩展触发阈值（0.8 = 80%）
	ExtendStrategy   string  // 扩展策略："fixed", "percentage", "double"
	ExtendSize       uint32  // 固定扩展大小（当ExtendStrategy="fixed"时）
	ExtendPercentage float64 // 百分比扩展（当ExtendStrategy="percentage"时）
	MaxTotalObjects  uint32  // 最大总对象数限制（0=无限制）
}

// Snapshot 完整的物理状态快照
type Snapshot struct {
	FrameNumber uint64
	Timestamp   time.Time
	Objects     []ObjectSyncData
	Changed     []uint32 // 变化对象的ObjectID列表
}

// OperationType 操作类型枚举
type OperationType uint32

const (
	OpMove     OperationType = 1 // 移动
	OpAttack   OperationType = 2 // 攻击
	OpUse      OperationType = 3 // 使用物品
	OpInteract OperationType = 4 // 交互
	OpJump     OperationType = 5 // 跳跃
	OpSkill    OperationType = 6 // 释放技能
	// 可扩展更多操作类型
)

// OperationCommand 操作命令（64字节，内存对齐）
// OperationCommand 操作命令（64字节，内存对齐）
type OperationCommand struct {
	Type     uint32  // 操作类型 (OperationType)
	SourceID uint32  // 发起者对象ID
	TargetID uint32  // 目标对象ID（0表示无目标）
	Param    float32 // 参数（强度、方向等）
	Seq      uint32  // 序列号（生产者分配，单调递增）
}

// OperationHeader 操作内存块头部
type OperationHeader struct {
	Capacity   uint32 // 环形缓冲区容量（固定）
	ReadIndex  uint32 // 消费者读位置（Go端）
	WriteIndex uint32 // 生产者写位置（Unity端）
	MaxSeq     uint32 // 生产者写入的最新序列号（用于检测32位环绕）
}

// OperationMemoryBlockConfig 操作内存块配置
type OperationMemoryBlockConfig struct {
	Name      string // 共享内存名称
	Capacity  uint32 // 最大命令数量（建议256-1024）
	EnableLog bool   // 是否启用日志
}

type ConnectorConfig struct {
	// 物理状态同步配置
	SyncConfig extensiveMemoryBlockConfig
	FrameRate  uint32 // 物理同步帧率（FPS）

	// 操作命令配置
	OpConfig    OperationMemoryBlockConfig
	PollingRate uint32 // 命令轮询频率（Hz）

	// 通用配置
	EnableLogging bool
	LogInterval   time.Duration
}

// PhysicalObjectProvider 物理对象提供者接口
type PhysicalObjectProvider interface {
	// GetObjects 获取当前物理对象状态
	// 返回：对象列表，变化对象ID列表
	GetObjects() ([]ObjectSyncData, []uint64)
}

// OperationCommandHandler 操作命令处理器接口
type OperationCommandHandler interface {
	// HandleCommands 处理从Unity接收的操作命令
	HandleCommands([]OperationCommand) error
}

// ConnectorStats 连接器统计信息
type ConnectorStats struct {
	TotalFrames          uint64
	TotalCommands        uint64
	AvgFrameTime         time.Duration
	AvgCommandLatency    time.Duration
	LastSyncTimestamp    time.Time
	LastCommandTimestamp time.Time
}

// DefaultConnectorConfig 连接器默认配置
var DefaultConnectorConfig = ConnectorConfig{
	SyncConfig: extensiveDefaultConfig,
	FrameRate:  60, // 默认60FPS物理同步

	OpConfig:    operationDefaultConfig,
	PollingRate: 100, // 默认100Hz命令轮询

	EnableLogging: true,
	LogInterval:   time.Second * 5,
}
