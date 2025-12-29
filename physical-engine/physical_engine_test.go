// physical_engine_test.go
package physicalengine_test

import (
	"testing"
	"time"

	physicalengine "server/physical-engine"

	"github.com/jakecoffman/cp/v2"
)

func TestBasicPhysicsSimulation(t *testing.T) {
	// 创建物理引擎配置 - 修复重力方向
	config := physicalengine.PhysicsEngineConfig{
		Gravity:         cp.Vector{X: 0, Y: -98.1}, // 修复：使用负值，重力向下
		TimeStep:        1.0 / 60.0,                // 60 FPS的时间步长
		Iterations:      10,                        // 碰撞迭代次数
		EnableDebugDraw: true,
	}

	// 创建物理引擎
	engine := physicalengine.NewPhysicsEngine(config)
	defer engine.Cleanup()

	// 创建地面 - 调整位置
	_, err := engine.CreateStaticBox("ground", "地面", 0, -10, 20, 1, map[string]interface{}{ // 降低地面位置
		"color": "green",
	})
	if err != nil {
		t.Fatalf("创建地面失败: %v", err)
	}

	// 创建测试物体 - 增加起始高度
	_, err = engine.CreateDynamicBox("test_box", "测试盒子", 0, 20, 1, 1, 1.0, map[string]interface{}{ // 提高起始高度
		"color": "red",
	})
	if err != nil {
		t.Fatalf("创建盒子失败: %v", err)
	}

	// 启动引擎
	engine.Start()

	// 模拟物理过程 - 增加模拟时间
	for i := 0; i < 600; i++ { // 模拟10秒（600帧）
		engine.Update()
		time.Sleep(time.Second / 60)

		// 每60帧打印一次位置
		if i%60 == 0 {
			x, y, _ := engine.GetObjectPosition("test_box")
			t.Logf("第%d帧: 位置(%.2f, %.2f)", i, x, y)
		}
	}

	// 检查最终位置
	x, y, ok := engine.GetObjectPosition("test_box")
	if !ok {
		t.Fatal("无法获取物体位置")
	}

	// 验证盒子应该落在地面上（地面在y=-10，盒子应该在y≈-9左右）
	if y > -9 {
		t.Errorf("盒子未正确落地，最终位置: (%.2f, %.2f)，期望y≈-9", x, y)
	} else {
		t.Logf("物理模拟成功，盒子落地位置: (%.2f, %.2f)", x, y)
	}
}

func TestSharedMemoryIntegration(t *testing.T) {
	// 创建共享内存物理引擎
	config := physicalengine.PhysicsEngineConfig{
		Gravity:         cp.Vector{X: 0, Y: -9.8}, // 修复：使用 cp.Vector
		TimeStep:        1.0 / 60.0,               // 修复：使用 TimeStep 而不是 FPS
		Iterations:      10,
		EnableDebugDraw: true,
	}

	engine, err := physicalengine.NewSharedMemoryPhysicsEngine(config, "TestSharedMemory")
	if err != nil {
		t.Fatalf("创建共享内存物理引擎失败: %v", err)
	}
	defer engine.Close()

	// 启动引擎（在后台运行）
	go engine.Run()

	// 等待引擎初始化
	time.Sleep(100 * time.Millisecond)

	// 测试输入处理
	t.Log("共享内存物理引擎启动成功，等待Unity连接...")

	// 让引擎运行一段时间
	time.Sleep(2 * time.Second)

	t.Log("测试完成")
}
