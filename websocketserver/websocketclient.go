package websocketserver

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// 客户端状态
type ClientStatus int32

const (
	StatusDisconnected ClientStatus = iota
	StatusConnecting
	StatusConnected
	StatusReconnecting
	StatusShuttingDown
)

// 增强的客户端结构
type WebSocketClient struct {
	conn     *websocket.Conn
	server   string
	name     string
	autoMode bool

	// 连接管理
	status      ClientStatus
	mu          sync.RWMutex
	stopChan    chan struct{}
	errChan     chan error
	reconnectCh chan struct{}

	// 统计信息
	messageCount      uint64
	lastPongTime      time.Time
	reconnectAttempts int
}

// 客户端初始化
func WebSocketClientInit(server, name string, autoMode bool) {
	client := &WebSocketClient{
		server:      server,
		name:        name,
		autoMode:    autoMode,
		stopChan:    make(chan struct{}),
		errChan:     make(chan error, 10),
		reconnectCh: make(chan struct{}, 1),
		status:      StatusDisconnected,
	}

	// 设置连接关闭处理器
	//client.setupCloseHandler()

	// 启动客户端主循环
	client.run()
}

// 设置连接关闭处理器
func (c *WebSocketClient) setupCloseHandler() {
	// 处理Ctrl+C信号
	go func() {
		// 这里可以添加信号处理，但Go的WebSocket库会自动处理连接关闭
	}()
}

// 客户端主循环
func (c *WebSocketClient) run() {
	defer c.cleanup()

	// 初始连接
	if err := c.connectWithRetry(); err != nil {
		log.Fatalf("初始连接失败: %v", err)
		return
	}

	// 启动各个协程
	var wg sync.WaitGroup

	// 消息接收协程
	wg.Add(1)
	go c.messageReceiver(&wg)

	// 心跳检测协程
	wg.Add(1)
	go c.heartbeatMonitor(&wg)

	// 错误监控协程
	wg.Add(1)
	go c.errorMonitor(&wg)

	// 根据模式启动发送逻辑
	if c.autoMode {
		log.Println("启动自动模式")
		c.autoSendMode()
	} else {
		log.Println("启动交互模式")
		c.interactiveMode()
	}

	// 等待所有协程结束
	c.setStatus(StatusShuttingDown)
	close(c.stopChan)
	wg.Wait()
}

// 带重试的连接
func (c *WebSocketClient) connectWithRetry() error {
	maxRetries := 5
	retryDelay := time.Second * 2

	for attempt := 0; attempt < maxRetries; attempt++ {
		c.setStatus(StatusConnecting)
		log.Printf("尝试连接服务器 (尝试 %d/%d)...", attempt+1, maxRetries)

		err := c.connect()
		if err == nil {
			c.setStatus(StatusConnected)
			c.reconnectAttempts = 0
			log.Printf("连接服务器成功")
			return nil
		}

		log.Printf("连接失败: %v", err)

		if attempt < maxRetries-1 {
			log.Printf("等待 %v 后重试...", retryDelay)
			time.Sleep(retryDelay)
			retryDelay *= 2 // 指数退避
		}
	}

	return fmt.Errorf("连接失败，已达到最大重试次数")
}

// 连接到服务器
func (c *WebSocketClient) connect() error {
	url := fmt.Sprintf("ws://%s/ws?name=%s", c.server, c.name)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.lastPongTime = time.Now()
	c.mu.Unlock()

	return nil
}

// 设置客户端状态
func (c *WebSocketClient) setStatus(status ClientStatus) {
	atomic.StoreInt32((*int32)(&c.status), int32(status))
}

// 获取客户端状态
func (c *WebSocketClient) getStatus() ClientStatus {
	return ClientStatus(atomic.LoadInt32((*int32)(&c.status)))
}

// 消息接收协程
func (c *WebSocketClient) messageReceiver(wg *sync.WaitGroup) {
	defer wg.Done()
	defer log.Println("消息接收协程结束")

	for {
		select {
		case <-c.stopChan:
			return
		default:
			if c.getStatus() != StatusConnected {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			c.mu.RLock()
			conn := c.conn
			c.mu.RUnlock()

			if conn == nil {
				c.errChan <- fmt.Errorf("连接对象为空")
				return
			}

			var msg Message
			err := conn.ReadJSON(&msg)
			if err != nil {
				c.errChan <- fmt.Errorf("读取消息错误: %v", err)
				return
			}

			atomic.AddUint64(&c.messageCount, 1)
			c.handleMessage(msg)
		}
	}
}

// 处理接收到的消息
func (c *WebSocketClient) handleMessage(msg Message) {
	// 更新最后收到pong的时间
	if msg.Type == "pong" {
		c.mu.Lock()
		c.lastPongTime = time.Now()
		c.mu.Unlock()
	}

	// 根据消息类型格式化显示
	switch msg.Type {
	case "user_list":
		if users, ok := msg.Content.([]interface{}); ok {
			fmt.Printf("\n=== 在线用户 (%d人) ===\n", len(users))
			for i, user := range users {
				fmt.Printf("%d. %s\n", i+1, user)
			}
			fmt.Println("=====================")
		}
	case "user_join", "user_leave":
		fmt.Printf("[系统] %s\n", msg.Content)
	case "chat":
		fmt.Printf("[聊天] %s\n", msg.Content)
	case "move":
		fmt.Printf("[移动] %s\n", msg.Content)
	case "pong":
		fmt.Printf("[心跳] 服务器响应: %v\n", msg.Content)
	default:
		fmt.Printf("[%s] %v\n", msg.Type, msg.Content)
	}
}

// 心跳检测协程
func (c *WebSocketClient) heartbeatMonitor(wg *sync.WaitGroup) {
	defer wg.Done()
	defer log.Println("心跳检测协程结束")

	heartbeatTicker := time.NewTicker(10 * time.Second) // 每10秒发送一次ping
	defer heartbeatTicker.Stop()

	healthCheckTicker := time.NewTicker(30 * time.Second) // 每30秒检查一次连接健康度
	defer healthCheckTicker.Stop()

	for {
		select {
		case <-c.stopChan:
			return
		case <-heartbeatTicker.C:
			if c.getStatus() == StatusConnected {
				c.sendPing()
			}
		case <-healthCheckTicker.C:
			if c.getStatus() == StatusConnected {
				c.checkConnectionHealth()
			}
		}
	}
}

// 发送ping消息
func (c *WebSocketClient) sendPing() {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn != nil {
		pingMsg := map[string]interface{}{
			"type":    "ping",
			"content": fmt.Sprintf("心跳检测 %s", time.Now().Format("15:04:05")),
			"from":    c.name,
		}
		if err := conn.WriteJSON(pingMsg); err != nil {
			c.errChan <- fmt.Errorf("发送ping失败: %v", err)
		}
	}
}

// 检查连接健康度
func (c *WebSocketClient) checkConnectionHealth() {
	c.mu.RLock()
	lastPong := c.lastPongTime
	c.mu.RUnlock()

	if time.Since(lastPong) > 60*time.Second {
		c.errChan <- fmt.Errorf("连接健康检查失败: 超过60秒未收到pong响应")
	}
}

// 错误监控协程
func (c *WebSocketClient) errorMonitor(wg *sync.WaitGroup) {
	defer wg.Done()
	defer log.Println("错误监控协程结束")

	for {
		select {
		case <-c.stopChan:
			return
		case err := <-c.errChan:
			log.Printf("检测到错误: %v", err)
			c.handleError(err)
		}
	}
}

// 处理错误
func (c *WebSocketClient) handleError(err error) {
	if c.getStatus() == StatusShuttingDown {
		return
	}

	log.Printf("处理连接错误: %v", err)

	// 尝试重连
	c.setStatus(StatusReconnecting)
	c.reconnectAttempts++

	if c.reconnectAttempts > 10 {
		log.Fatalf("重连尝试次数过多，退出程序")
		return
	}

	log.Printf("尝试重新连接 (第%d次)...", c.reconnectAttempts)
	if err := c.connectWithRetry(); err != nil {
		log.Printf("重连失败: %v", err)
		// 继续等待下一次错误处理
	} else {
		log.Println("重连成功")
	}
}

// 发送消息到服务器（线程安全）
func (c *WebSocketClient) sendMessage(msgType string, content interface{}) error {
	if c.getStatus() != StatusConnected {
		return fmt.Errorf("客户端未连接")
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("连接对象为空")
	}

	msg := map[string]interface{}{
		"type":    msgType,
		"content": content,
		"from":    c.name,
		"time":    time.Now().Format("2006-01-02 15:04:05"),
	}

	return conn.WriteJSON(msg)
}

// 自动发送模式
func (c *WebSocketClient) autoSendMode() {
	ticker := time.NewTicker(8 * time.Second) // 改为8秒一次，避免与心跳冲突
	defer ticker.Stop()

	counter := 0
	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			if c.getStatus() != StatusConnected {
				continue
			}

			counter++

			switch counter % 4 {
			case 0:
				c.sendMessage("ping", fmt.Sprintf("自动ping消息 #%d", counter))
			case 1:
				c.sendMessage("chat", fmt.Sprintf("大家好！这是来自 %s 的自动消息 #%d", c.name, counter))
			case 2:
				moveData := map[string]int{
					"x": counter * 10,
					"y": counter * 5,
				}
				c.sendMessage("move", moveData)
			case 3:
				c.sendMessage("chat", fmt.Sprintf("当前是第 %d 轮测试，已接收 %d 条消息", counter, atomic.LoadUint64(&c.messageCount)))
			}
		}
	}
}

// 交互式模式（增强版本）
func (c *WebSocketClient) interactiveMode() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("=== WebSocket客户端交互模式 ===")
	fmt.Println("命令说明:")
	fmt.Println("  ping          - 发送ping消息")
	fmt.Println("  chat <内容>   - 发送聊天消息（所有人可见）")
	fmt.Println("  move x y      - 发送移动消息")
	fmt.Println("  status        - 查看连接状态")
	fmt.Println("  users         - 查看在线用户")
	fmt.Println("  quit          - 退出客户端")
	fmt.Println("==============================")

	for {
		select {
		case <-c.stopChan:
			return
		default:
			if c.getStatus() != StatusConnected {
				fmt.Printf("连接状态: %s，等待连接恢复...\n", c.getStatusString())
				time.Sleep(2 * time.Second)
				continue
			}

			fmt.Print("请输入命令: ")
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)

			if input == "quit" {
				return
			}

			c.handleCommand(input)
		}
	}
}

// 处理用户命令
func (c *WebSocketClient) handleCommand(input string) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return
	}

	switch parts[0] {
	case "ping":
		if err := c.sendMessage("ping", "手动ping请求"); err != nil {
			fmt.Printf("发送失败: %v\n", err)
		} else {
			fmt.Println("已发送ping消息")
		}

	case "chat":
		if len(parts) > 1 {
			content := strings.Join(parts[1:], " ")
			if err := c.sendMessage("chat", content); err != nil {
				fmt.Printf("发送失败: %v\n", err)
			} else {
				fmt.Printf("已发送聊天消息: %s\n", content)
			}
		} else {
			fmt.Println("用法: chat <消息内容>")
		}

	case "move":
		if len(parts) == 3 {
			if err := c.sendMessage("move", map[string]string{
				"x": parts[1],
				"y": parts[2],
			}); err != nil {
				fmt.Printf("发送失败: %v\n", err)
			} else {
				fmt.Printf("已发送移动消息: x=%s, y=%s\n", parts[1], parts[2])
			}
		} else {
			fmt.Println("用法: move <x坐标> <y坐标>")
		}

	case "status":
		c.showStatus()

	case "users":
		fmt.Println("用户列表请求功能已集成到系统消息中")

	default:
		fmt.Println("未知命令，请输入: ping, chat, move, status, users 或 quit")
	}
}

// 显示连接状态
func (c *WebSocketClient) showStatus() {
	c.mu.RLock()
	defer c.mu.RUnlock()

	fmt.Println("\n=== 客户端状态 ===")
	fmt.Printf("连接状态: %s\n", c.getStatusString())
	fmt.Printf("服务器: %s\n", c.server)
	fmt.Printf("客户端名: %s\n", c.name)
	fmt.Printf("已接收消息: %d\n", atomic.LoadUint64(&c.messageCount))
	fmt.Printf("重连尝试次数: %d\n", c.reconnectAttempts)
	fmt.Printf("最后pong时间: %s\n", c.lastPongTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("连接持续时间: %v\n", time.Since(c.lastPongTime))
	fmt.Println("=================")
}

// 获取状态字符串
func (c *WebSocketClient) getStatusString() string {
	switch c.getStatus() {
	case StatusDisconnected:
		return "未连接"
	case StatusConnecting:
		return "连接中"
	case StatusConnected:
		return "已连接"
	case StatusReconnecting:
		return "重连中"
	case StatusShuttingDown:
		return "关闭中"
	default:
		return "未知状态"
	}
}

// 清理资源
func (c *WebSocketClient) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	close(c.errChan)
	close(c.reconnectCh)
	log.Println("客户端资源清理完成")
}
