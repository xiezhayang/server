package physicalengine

import (
	"fmt"
	"log"
	"time"

	"github.com/jakecoffman/cp/v2"
)

// 物理引擎状态
type PhysicsEngineState int

const (
	EngineStopped PhysicsEngineState = iota
	EngineRunning
	EnginePaused
)

// 物理对象类型
type PhysicsObjectType int

const (
	ObjectTypeStatic PhysicsObjectType = iota
	ObjectTypeDynamic
	ObjectTypeKinematic
)

// 物理对象
type PhysicsObject struct {
	ID         string
	Name       string
	Body       *cp.Body
	Shape      *cp.Shape
	ObjectType PhysicsObjectType
	Properties map[string]interface{}
	UserData   interface{}
}

// 碰撞事件
type CollisionEvent struct {
	ObjectA     *PhysicsObject
	ObjectB     *PhysicsObject
	Normal      cp.Vector
	Penetration float64
	Time        time.Time
}

// 物理引擎配置
type PhysicsEngineConfig struct {
	Gravity         cp.Vector
	TimeStep        float64
	Iterations      int
	Damping         float64
	EnableSleep     bool
	SleepThreshold  float64
	CollisionSlop   float64
	EnableDebugDraw bool
}

// 物理引擎
type PhysicsEngine struct {
	space         *cp.Space
	config        PhysicsEngineConfig
	state         PhysicsEngineState
	objects       map[string]*PhysicsObject
	collisionChan chan *CollisionEvent
	debugDrawChan chan func()
	lastUpdate    time.Time
	frameCount    uint64
}

// 默认配置
var DefaultConfig = PhysicsEngineConfig{
	Gravity:         cp.Vector{X: 0, Y: 98.1}, // 默认重力向下
	TimeStep:        1.0 / 60.0,               // 60 FPS
	Iterations:      10,
	Damping:         0.95,
	EnableSleep:     true,
	SleepThreshold:  0.5,
	CollisionSlop:   0.1,
	EnableDebugDraw: false,
}

// 创建新的物理引擎
func NewPhysicsEngine(config PhysicsEngineConfig) *PhysicsEngine {
	if config.TimeStep == 0 {
		config.TimeStep = DefaultConfig.TimeStep
	}
	if config.Iterations == 0 {
		config.Iterations = DefaultConfig.Iterations
	}

	engine := &PhysicsEngine{
		space:         cp.NewSpace(),
		config:        config,
		state:         EngineStopped,
		objects:       make(map[string]*PhysicsObject),
		collisionChan: make(chan *CollisionEvent, 100),
		debugDrawChan: make(chan func(), 10),
		lastUpdate:    time.Now(),
	}

	// 配置物理空间
	engine.space.SetGravity(config.Gravity)
	engine.space.SetDamping(config.Damping)
	engine.space.Iterations = uint(config.Iterations)
	engine.space.SetCollisionSlop(config.CollisionSlop)

	if config.EnableSleep {
		engine.space.IdleSpeedThreshold = config.SleepThreshold
		engine.space.SleepTimeThreshold = 0.5
	}

	// 设置碰撞回调
	engine.setupCollisionHandlers()

	return engine
}

// 设置碰撞处理器
func (engine *PhysicsEngine) setupCollisionHandlers() {
	// 创建默认碰撞处理器（处理所有类型的碰撞）
	handler := engine.space.NewWildcardCollisionHandler(0)

	// 设置开始碰撞回调
	handler.BeginFunc = func(arb *cp.Arbiter, space *cp.Space, data interface{}) bool {
		// 获取碰撞的两个形状
		a, b := arb.Shapes()
		if a == nil || b == nil {
			return true
		}

		// 查找对应的物理对象
		objA := engine.findObjectByShape(a)
		objB := engine.findObjectByShape(b)
		if objA == nil || objB == nil {
			return true
		}

		// 获取碰撞信息（修正后的正确API）
		contactSet := arb.ContactPointSet()
		if contactSet.Count > 0 {
			normal := contactSet.Normal
			penetration := contactSet.Points[0].Distance

			// 发送碰撞事件
			event := &CollisionEvent{
				ObjectA:     objA,
				ObjectB:     objB,
				Normal:      normal,
				Penetration: penetration,
				Time:        time.Now(),
			}

			select {
			case engine.collisionChan <- event:
				// 成功发送事件
			default:
				log.Printf("碰撞事件通道已满，丢弃事件")
			}
		}

		return true // 允许碰撞
	}
}

// 通过形状查找物理对象
func (engine *PhysicsEngine) findObjectByShape(shape *cp.Shape) *PhysicsObject {
	for _, obj := range engine.objects {
		if obj.Shape == shape {
			return obj
		}
	}
	return nil
}

// 启动物理引擎
func (engine *PhysicsEngine) Start() {
	if engine.state == EngineRunning {
		return
	}

	engine.state = EngineRunning
	engine.lastUpdate = time.Now()
	log.Printf("物理引擎启动")
}

// 停止物理引擎
func (engine *PhysicsEngine) Stop() {
	if engine.state == EngineStopped {
		return
	}

	engine.state = EngineStopped
	log.Printf("物理引擎停止")
}

// 暂停物理引擎
func (engine *PhysicsEngine) Pause() {
	if engine.state == EnginePaused {
		return
	}

	engine.state = EnginePaused
	log.Printf("物理引擎暂停")
}

// 恢复物理引擎
func (engine *PhysicsEngine) Resume() {
	if engine.state != EnginePaused {
		return
	}

	engine.state = EngineRunning
	engine.lastUpdate = time.Now()
	log.Printf("物理引擎恢复")
}

// 更新物理引擎
func (engine *PhysicsEngine) Update() {
	if engine.state != EngineRunning {
		return
	}

	currentTime := time.Now()
	elapsed := currentTime.Sub(engine.lastUpdate).Seconds()

	// 限制最小时间步长，避免过度物理计算
	if elapsed < engine.config.TimeStep {
		return
	}

	// 计算需要更新的步数
	steps := int(elapsed / engine.config.TimeStep)
	if steps > 10 { // 限制最大步数，避免卡顿
		steps = 10
		log.Printf("物理更新步数过多: %d，限制为10步", steps)
	}

	// 执行物理更新
	for i := 0; i < steps; i++ {
		engine.space.Step(engine.config.TimeStep)
		engine.frameCount++
	}

	engine.lastUpdate = currentTime.Add(-time.Duration((elapsed - float64(steps)*engine.config.TimeStep) * float64(time.Second)))
}

// 创建静态物体（地面、墙壁等）
func (engine *PhysicsEngine) CreateStaticBox(id, name string, x, y, width, height float64, properties map[string]interface{}) (*PhysicsObject, error) {
	if _, exists := engine.objects[id]; exists {
		return nil, fmt.Errorf("物体ID已存在: %s", id)
	}

	// 创建静态物体
	body := cp.NewStaticBody()
	body.SetPosition(cp.Vector{X: x, Y: y})

	// 创建矩形形状
	shape := cp.NewBox(body, width, height, 0)
	shape.SetFriction(0.7)
	shape.SetElasticity(0.1)

	// 添加到空间
	engine.space.AddBody(body)
	engine.space.AddShape(shape)

	obj := &PhysicsObject{
		ID:         id,
		Name:       name,
		Body:       body,
		Shape:      shape,
		ObjectType: ObjectTypeStatic,
		Properties: properties,
	}

	engine.objects[id] = obj
	return obj, nil
}

// 创建动态物体（可移动的物体）
func (engine *PhysicsEngine) CreateDynamicBox(id, name string, x, y, width, height, mass float64, properties map[string]interface{}) (*PhysicsObject, error) {
	if _, exists := engine.objects[id]; exists {
		return nil, fmt.Errorf("物体ID已存在: %s", id)
	}

	// 计算转动惯量（矩形的转动惯量）
	moment := cp.MomentForBox(mass, width, height)

	// 创建动态物体
	body := cp.NewBody(mass, moment)
	body.SetPosition(cp.Vector{X: x, Y: y})

	// 创建矩形形状
	shape := cp.NewBox(body, width, height, 0)
	shape.SetFriction(0.5)
	shape.SetElasticity(0.3)

	// 添加到空间
	engine.space.AddBody(body)
	engine.space.AddShape(shape)

	obj := &PhysicsObject{
		ID:         id,
		Name:       name,
		Body:       body,
		Shape:      shape,
		ObjectType: ObjectTypeDynamic,
		Properties: properties,
	}

	engine.objects[id] = obj
	return obj, nil
}

// 创建圆形动态物体
func (engine *PhysicsEngine) CreateDynamicCircle(id, name string, x, y, radius, mass float64, properties map[string]interface{}) (*PhysicsObject, error) {
	if _, exists := engine.objects[id]; exists {
		return nil, fmt.Errorf("物体ID已存在: %s", id)
	}

	// 计算转动惯量（圆形的转动惯量）
	moment := cp.MomentForCircle(mass, 0, radius, cp.Vector{})

	// 创建动态物体
	body := cp.NewBody(mass, moment)
	body.SetPosition(cp.Vector{X: x, Y: y})

	// 创建圆形形状
	shape := cp.NewCircle(body, radius, cp.Vector{})
	shape.SetFriction(0.5)
	shape.SetElasticity(0.6)

	// 添加到空间
	engine.space.AddBody(body)
	engine.space.AddShape(shape)

	obj := &PhysicsObject{
		ID:         id,
		Name:       name,
		Body:       body,
		Shape:      shape,
		ObjectType: ObjectTypeDynamic,
		Properties: properties,
	}

	engine.objects[id] = obj
	return obj, nil
}

// 创建静态线段（平台、边界等）
func (engine *PhysicsEngine) CreateStaticSegment(id, name string, x1, y1, x2, y2, radius float64, properties map[string]interface{}) (*PhysicsObject, error) {
	if _, exists := engine.objects[id]; exists {
		return nil, fmt.Errorf("物体ID已存在: %s", id)
	}

	// 创建静态物体
	body := cp.NewStaticBody()

	// 创建线段形状
	shape := cp.NewSegment(body, cp.Vector{X: x1, Y: y1}, cp.Vector{X: x2, Y: y2}, radius)
	shape.SetFriction(0.7)
	shape.SetElasticity(0.1)

	// 添加到空间
	engine.space.AddBody(body)
	engine.space.AddShape(shape)

	obj := &PhysicsObject{
		ID:         id,
		Name:       name,
		Body:       body,
		Shape:      shape,
		ObjectType: ObjectTypeStatic,
		Properties: properties,
	}

	engine.objects[id] = obj
	return obj, nil
}

// 获取物体
func (engine *PhysicsEngine) GetObject(id string) *PhysicsObject {
	return engine.objects[id]
}

// 获取所有物体
func (engine *PhysicsEngine) GetAllObjects() []*PhysicsObject {
	objects := make([]*PhysicsObject, 0, len(engine.objects))
	for _, obj := range engine.objects {
		objects = append(objects, obj)
	}
	return objects
}

// 删除物体
func (engine *PhysicsEngine) RemoveObject(id string) bool {
	obj, exists := engine.objects[id]
	if !exists {
		return false
	}

	// 从物理空间中移除
	if obj.Body != nil {
		engine.space.RemoveBody(obj.Body)
	}
	if obj.Shape != nil {
		engine.space.RemoveShape(obj.Shape)
	}

	delete(engine.objects, id)
	return true
}

// 设置物体位置
func (engine *PhysicsEngine) SetObjectPosition(id string, x, y float64) bool {
	obj := engine.GetObject(id)
	if obj == nil {
		return false
	}

	obj.Body.SetPosition(cp.Vector{X: x, Y: y})
	return true
}

// 获取物体位置
func (engine *PhysicsEngine) GetObjectPosition(id string) (float64, float64, bool) {
	obj := engine.GetObject(id)
	if obj == nil {
		return 0, 0, false
	}

	pos := obj.Body.Position()
	return pos.X, pos.Y, true
}

// 施加力到物体
func (engine *PhysicsEngine) ApplyForce(id string, fx, fy float64) bool {
	obj := engine.GetObject(id)
	if obj == nil || obj.ObjectType != ObjectTypeDynamic {
		log.Printf("ApplyForce: 物体不存在或不是动态物体: %s", id)
		return false
	}

	// 修正：使用正确的API名称
	obj.Body.ApplyForceAtWorldPoint(cp.Vector{X: fx, Y: fy}, cp.Vector{})
	return true
}

// 施加冲量到物体
func (engine *PhysicsEngine) ApplyImpulse(id string, fx, fy float64) bool {
	obj := engine.GetObject(id)
	if obj == nil || obj.ObjectType != ObjectTypeDynamic {
		log.Printf("ApplyForce: 物体不存在或不是动态物体: %s", id)
		return false
	}

	// 修正：使用正确的API名称
	obj.Body.ApplyImpulseAtWorldPoint(cp.Vector{X: fx, Y: fy}, cp.Vector{})
	return true
}

// 设置物体速度
func (engine *PhysicsEngine) SetObjectVelocity(id string, vx, vy float64) bool {
	obj := engine.GetObject(id)
	if obj == nil || obj.ObjectType != ObjectTypeDynamic {
		return false
	}

	// 修正：直接传递两个浮点数参数
	obj.Body.SetVelocity(vx, vy)
	return true
}

// 获取物体速度
func (engine *PhysicsEngine) GetObjectVelocity(id string) (float64, float64, bool) {
	obj := engine.GetObject(id)
	if obj == nil {
		return 0, 0, false
	}

	vel := obj.Body.Velocity()
	return vel.X, vel.Y, true
}

// 获取碰撞事件通道
func (engine *PhysicsEngine) GetCollisionChannel() <-chan *CollisionEvent {
	return engine.collisionChan
}

// 获取引擎状态
func (engine *PhysicsEngine) GetState() PhysicsEngineState {
	return engine.state
}

// 获取帧计数
func (engine *PhysicsEngine) GetFrameCount() uint64 {
	return engine.frameCount
}

// 获取物体数量
func (engine *PhysicsEngine) GetObjectCount() int {
	return len(engine.objects)
}

// 设置重力
func (engine *PhysicsEngine) SetGravity(gx, gy float64) {
	engine.config.Gravity = cp.Vector{X: gx, Y: gy}
	engine.space.SetGravity(engine.config.Gravity)
}

// 获取重力
func (engine *PhysicsEngine) GetGravity() (float64, float64) {
	return engine.config.Gravity.X, engine.config.Gravity.Y
}

// 清理引擎
func (engine *PhysicsEngine) Cleanup() {
	engine.Stop()

	// 清空所有物体
	for id := range engine.objects {
		engine.RemoveObject(id)
	}

	// 关闭通道
	close(engine.collisionChan)
	close(engine.debugDrawChan)

	log.Printf("物理引擎清理完成")
}

// 工具函数：创建简单的平台场景
func (engine *PhysicsEngine) CreatePlatformScene() {
	// 创建地面
	engine.CreateStaticBox("ground", "地面", 400, 590, 800, 20, map[string]interface{}{
		"color": "brown",
	})

	// 创建左侧墙壁
	engine.CreateStaticBox("left_wall", "左侧墙壁", 0, 300, 20, 600, map[string]interface{}{
		"color": "gray",
	})

	// 创建右侧墙壁
	engine.CreateStaticBox("right_wall", "右侧墙壁", 800, 300, 20, 600, map[string]interface{}{
		"color": "gray",
	})

	// 创建几个平台
	engine.CreateStaticBox("platform1", "平台1", 200, 450, 150, 10, map[string]interface{}{
		"color": "green",
	})

	engine.CreateStaticBox("platform2", "平台2", 500, 350, 150, 10, map[string]interface{}{
		"color": "green",
	})

	// 创建一些动态物体
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("box%d", i+1)
		engine.CreateDynamicBox(id, fmt.Sprintf("箱子%d", i+1),
			float64(100+i*120), 100, 30, 30, 1.0,
			map[string]interface{}{
				"color": "blue",
			})
	}

	// 创建一些圆形物体
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("ball%d", i+1)
		engine.CreateDynamicCircle(id, fmt.Sprintf("球%d", i+1),
			float64(150+i*200), 50, 15, 0.5,
			map[string]interface{}{
				"color": "red",
			})
	}
}

// 示例使用函数
func ExampleUsage() {
	// 创建物理引擎
	config := DefaultConfig
	config.Gravity = cp.Vector{X: 0, Y: 98.1} // 设置重力
	engine := NewPhysicsEngine(config)

	// 创建场景
	engine.CreatePlatformScene()

	// 启动引擎
	engine.Start()

	// 模拟运行
	for i := 0; i < 100; i++ {
		engine.Update()

		// 处理碰撞事件
		select {
		case collision := <-engine.GetCollisionChannel():
			log.Printf("碰撞: %s 和 %s", collision.ObjectA.Name, collision.ObjectB.Name)
		default:
			// 无碰撞事件
		}

		// 获取物体位置信息
		if obj := engine.GetObject("box1"); obj != nil {
			x, y, _ := engine.GetObjectPosition("box1")
			log.Printf("box1位置: (%.2f, %.2f)", x, y)
		}
	}

	// 清理
	engine.Cleanup()
}
