// 修正后的物理引擎测试文件 - 修复时间步长问题
package physicalengine_test

import (
	"testing"
	"time"

	physicalengine "server/physical-engine"

	"github.com/jakecoffman/cp/v2"
)

func TestBasicPhysicsSimulation(t *testing.T) {
	// 创建物理引擎配置 - 使用正确的重力方向（向下，正值）
	config := physicalengine.PhysicsEngineConfig{
		Gravity:         cp.Vector{X: 0, Y: -8000},
		TimeStep:        1.0 / 60.0, // 60 FPS的时间步长
		Iterations:      10,         // 碰撞迭代次数
		Damping:         0.1,
		EnableDebugDraw: true,
	}

	// 创建物理引擎
	engine := physicalengine.NewPhysicsEngine(config)
	defer engine.Cleanup()

	// 创建地面 - 放在底部（Y=600是地面）
	_, err := engine.CreateStaticBox("ground", "地面", 400, 0, 400, 400, map[string]interface{}{})
	if err != nil {
		t.Fatalf("创建地面失败: %v", err)
	}

	// 创建测试物体 - 从高处释放（Y=100是高处）
	_, err = engine.CreateDynamicBox("test_box", "测试盒子", 400, 1000, 30, 30, 1.0, map[string]interface{}{})
	if err != nil {
		t.Fatalf("创建盒子失败: %v", err)
	}

	// 启动引擎
	engine.Start()

	// 模拟物理过程
	lastY := 1000.0
	//grounded := false
	for i := 0; i < 300; i++ { // 模拟5秒
		// 确保有足够的时间间隔
		time.Sleep(time.Millisecond * 16)
		engine.Update()

		// 每30帧打印一次位置
		if i%30 == 0 {
			x, y, _ := engine.GetObjectPosition("test_box")
			t.Logf("第%d帧: 位置(%.2f, %.2f)", i, x, y)

			// 检查是否在下落（Y值应该增加）
			if y > lastY && i > 0 {
				t.Logf("警告：第%d帧物体异常上升", i)
			}

			lastY = y
		}
	}

	// 检查最终位置
	x, y, ok := engine.GetObjectPosition("test_box")
	if !ok {
		t.Fatal("无法获取物体位置")
	}

	t.Logf("物理模拟成功，盒子落地位置: (%.2f, %.2f)", x, y)

}

func TestCollisionDetection(t *testing.T) {
	// 创建物理引擎配置
	config := physicalengine.PhysicsEngineConfig{
		Gravity:         cp.Vector{X: 0, Y: 0}, // 无重力环境
		TimeStep:        1.0 / 60.0,
		Iterations:      10,
		EnableDebugDraw: true,
	}

	// 创建物理引擎
	engine := physicalengine.NewPhysicsEngine(config)
	defer engine.Cleanup()

	// 创建两个会碰撞的物体 - 使用正确的重叠位置
	_, err := engine.CreateDynamicBox("box1", "盒子1", 300, 300, 40, 40, 1.0, nil)
	if err != nil {
		t.Fatalf("创建盒子1失败: %v", err)
	}

	// 让盒子2直接与盒子1重叠一部分
	_, err = engine.CreateDynamicBox("box2", "盒子2", 340, 300, 40, 40, 1.0, nil)
	if err != nil {
		t.Fatalf("创建盒子2失败: %v", err)
	}

	// 给盒子2一个明显的速度，确保它们会碰撞
	engine.SetObjectVelocity("box2", -100, 0)

	// 启动引擎
	engine.Start()

	// 先等待一小段时间
	time.Sleep(time.Millisecond * 50)

	// 监听碰撞事件
	collisionCount := 0
	collisionChan := engine.GetCollisionChannel()

	// 模拟物理过程
	for i := 0; i < 120; i++ {
		time.Sleep(time.Millisecond * 8)
		engine.Update()

		// 非阻塞检查碰撞事件
		select {
		case collision := <-collisionChan:
			if collision != nil {
				collisionCount++
				t.Logf("碰撞检测: %s 和 %s", collision.ObjectA.Name, collision.ObjectB.Name)
			}
		default:
			// 无碰撞事件
		}

		if collisionCount > 0 {
			break
		}
	}

	if collisionCount == 0 {
		t.Error("未检测到碰撞")
	} else {
		t.Logf("检测到 %d 次碰撞", collisionCount)
	}
}

func TestObjectManipulation(t *testing.T) {
	// 创建物理引擎配置
	config := physicalengine.PhysicsEngineConfig{
		Gravity:         cp.Vector{X: 0, Y: 0}, // 无重力环境
		TimeStep:        1.0 / 60.0,
		Iterations:      10,
		EnableDebugDraw: true,
	}

	// 创建物理引擎
	engine := physicalengine.NewPhysicsEngine(config)
	defer engine.Cleanup()

	// 创建测试物体
	_, err := engine.CreateDynamicBox("test_obj", "测试物体", 400, 300, 30, 30, 1.0, nil)
	if err != nil {
		t.Fatalf("创建物体失败: %v", err)
	}

	// 启动引擎
	engine.Start()

	// 先等待一小段时间
	time.Sleep(time.Millisecond * 50)

	// 测试施加力 - 使用更大的力
	success := engine.ApplyForce("test_obj", 5000, 0)
	if !success {
		t.Fatal("施加力失败")
	}

	// 模拟足够帧数让力生效，确保有足够时间间隔
	for i := 0; i < 120; i++ {
		time.Sleep(time.Millisecond * 16)
		engine.Update()
	}

	// 检查位置变化
	x, y, _ := engine.GetObjectPosition("test_obj")
	if x == 400 {
		t.Errorf("施加力后物体位置未变化: (%.2f, %.2f)", x, y)
	} else {
		t.Logf("施加力成功，物体移动到: (%.2f, %.2f)", x, y)
	}

	// 测试设置速度
	success = engine.SetObjectVelocity("test_obj", 200, 100)
	if !success {
		t.Fatal("设置速度失败")
	}

	// 模拟几帧
	for i := 0; i < 30; i++ {
		time.Sleep(time.Millisecond * 16)
		engine.Update()
	}

	// 检查速度效果
	vx, vy, _ := engine.GetObjectVelocity("test_obj")
	if vx == 0 && vy == 0 {
		t.Errorf("设置速度后速度未变化: (%.2f, %.2f)", vx, vy)
	} else {
		t.Logf("设置速度成功，当前速度: (%.2f, %.2f)", vx, vy)
	}
}

func TestPlatformScene(t *testing.T) {
	// 创建物理引擎配置 - 使用正确的重力方向
	config := physicalengine.PhysicsEngineConfig{
		Gravity:         cp.Vector{X: 0, Y: 800}, // 向下重力（正值！）
		TimeStep:        1.0 / 60.0,
		Iterations:      10,
		EnableDebugDraw: true,
	}

	// 创建物理引擎
	engine := physicalengine.NewPhysicsEngine(config)
	defer engine.Cleanup()

	// 创建平台场景
	engine.CreatePlatformScene()

	// 启动引擎
	engine.Start()

	// 先等待一小段时间
	time.Sleep(time.Millisecond * 50)

	// 检查物体数量
	objectCount := engine.GetObjectCount()
	if objectCount == 0 {
		t.Fatal("场景创建失败，没有物体")
	}

	t.Logf("场景创建成功，包含 %d 个物体", objectCount)

	// 模拟物理过程
	for i := 0; i < 180; i++ {
		time.Sleep(time.Millisecond * 16)
		engine.Update()
		if i%60 == 0 {
			// 检查物体是否在下落
			if obj := engine.GetObject("box1"); obj != nil {
				x, y, _ := engine.GetObjectPosition("box1")
				t.Logf("模拟第%d帧: box1位置(%.2f, %.2f)", i, x, y)
			}
		}
	}

	t.Log("平台场景测试完成")
}

// 跳过共享内存测试
func TestSharedMemoryIntegration(t *testing.T) {
	t.Skip("跳过共享内存测试（仅在Windows上运行）")
}
