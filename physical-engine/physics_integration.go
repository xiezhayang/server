// physics_integration.go
package physicalengine

import (
	"time"
)

// 共享内存物理引擎集成
type SharedMemoryPhysicsEngine struct {
	physicsEngine *PhysicsEngine
	sharedMemory  *SharedMemoryManager
	playerObject  *PhysicsObject
}

// 创建共享内存物理引擎
func NewSharedMemoryPhysicsEngine(config PhysicsEngineConfig, shmName string) (*SharedMemoryPhysicsEngine, error) {
	// 创建物理引擎
	physicsEngine := NewPhysicsEngine(config)

	// 创建共享内存管理器
	sharedMemory, err := NewSharedMemoryManager(shmName, 1024, true)
	if err != nil {
		return nil, err
	}

	engine := &SharedMemoryPhysicsEngine{
		physicsEngine: physicsEngine,
		sharedMemory:  sharedMemory,
	}

	// 创建玩家物理对象
	playerObj, err := physicsEngine.CreateDynamicBox("player", "玩家", 0, 0, 1, 2, 1.0, map[string]interface{}{
		"color": "blue",
	})
	if err != nil {
		sharedMemory.Close()
		return nil, err
	}
	engine.playerObject = playerObj

	return engine, nil
}

// 运行物理引擎循环
func (engine *SharedMemoryPhysicsEngine) Run() {
	engine.physicsEngine.Start()

	ticker := time.NewTicker(time.Second / 60) // 60 FPS
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			engine.Update()
		}
	}
}

// 更新物理引擎
func (engine *SharedMemoryPhysicsEngine) Update() {
	// 读取Unity输入
	inputX, _, isJumping := engine.sharedMemory.ReadInput() // 移除未使用的inputY

	// 应用输入到物理对象
	if engine.playerObject != nil {
		// 水平移动力 - 转换为float64
		moveForce := float64(inputX * 100.0) // 转换为float64
		engine.physicsEngine.ApplyForce("player", moveForce, 0)

		// 跳跃
		if isJumping {
			// 检查是否在地面上
			if engine.isPlayerGrounded() {
				engine.physicsEngine.ApplyImpulse("player", 0, 300.0)
			}
		}

		// 更新物理引擎
		engine.physicsEngine.Update()

		// 获取物理对象位置
		x, y, _ := engine.physicsEngine.GetObjectPosition("player")
		vx, vy, _ := engine.physicsEngine.GetObjectVelocity("player")

		// 写入物理数据到共享内存
		engine.sharedMemory.WritePhysics(float32(x), float32(y), float32(vx), float32(vy))
	}
}

// 检查玩家是否在地面上
func (engine *SharedMemoryPhysicsEngine) isPlayerGrounded() bool {
	// 简单的接地检测
	_, y, _ := engine.physicsEngine.GetObjectPosition("player")
	return y <= 0.1 // 假设地面在y=0
}

// 关闭引擎
func (engine *SharedMemoryPhysicsEngine) Close() {
	if engine.physicsEngine != nil {
		engine.physicsEngine.Cleanup()
	}
	if engine.sharedMemory != nil {
		engine.sharedMemory.Close()
	}
}
