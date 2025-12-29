package websocketserver

import (
	//"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Message struct {
	Type    string      `json:"type"`
	Content interface{} `json:"content"`
	Time    string      `json:"time"`
	From    string      `json:"from,omitempty"` // 发送者信息
}

// 客户端信息
type Client struct {
	ID       string
	Name     string
	Conn     *websocket.Conn
	Addr     string
	JoinTime time.Time
}

// 连接池
type ConnectionPool struct {
	clients    map[*websocket.Conn]*Client // 连接 -> 客户端信息
	mutex      sync.RWMutex                // 读写锁，保证线程安全
	broadcast  chan Message                // 广播消息通道
	register   chan *Client                // 注册通道
	unregister chan *websocket.Conn        // 注销通道
}

// 全局连接池实例
var pool *ConnectionPool

// 初始化连接池
func init() {
	pool = &ConnectionPool{
		clients:    make(map[*websocket.Conn]*Client),
		broadcast:  make(chan Message, 100),
		register:   make(chan *Client),
		unregister: make(chan *websocket.Conn),
	}
	go pool.run()
}

// 连接池主循环
func (p *ConnectionPool) run() {
	for {
		select {
		case client := <-p.register:
			// 注册新客户端
			p.mutex.Lock()
			p.clients[client.Conn] = client
			p.mutex.Unlock()

			log.Printf("客户端 %s (%s) 加入，当前在线: %d", client.Name, client.ID, len(p.clients))

			// 广播新用户加入消息
			p.broadcastMessage(Message{
				Type:    "user_join",
				Content: fmt.Sprintf("用户 %s 加入了游戏", client.Name),
				Time:    time.Now().Format("2006-01-02 15:04:05"),
				From:    "系统",
			})

			// 发送在线用户列表给新用户
			p.sendUserList(client.Conn)

		case conn := <-p.unregister:
			// 注销客户端
			p.mutex.Lock()
			if client, exists := p.clients[conn]; exists {
				delete(p.clients, conn)
				conn.Close()

				log.Printf("客户端 %s (%s) 离开，当前在线: %d", client.Name, client.ID, len(p.clients))

				// 广播用户离开消息
				p.broadcastMessage(Message{
					Type:    "user_leave",
					Content: fmt.Sprintf("用户 %s 离开了游戏", client.Name),
					Time:    time.Now().Format("2006-01-02 15:04:05"),
					From:    "系统",
				})
			}
			p.mutex.Unlock()

		case message := <-p.broadcast:
			// 广播消息给所有客户端
			p.mutex.RLock()
			for conn := range p.clients {
				err := conn.WriteJSON(message)
				if err != nil {
					log.Printf("广播消息失败: %v", err)
					conn.Close()
					delete(p.clients, conn)
				}
			}
			p.mutex.RUnlock()
		}
	}
}

// 广播消息
func (p *ConnectionPool) broadcastMessage(message Message) {
	p.broadcast <- message
}

// 发送消息给特定客户端
func (p *ConnectionPool) sendToClient(conn *websocket.Conn, message Message) error {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	if _, exists := p.clients[conn]; exists {
		return conn.WriteJSON(message)
	}
	return fmt.Errorf("客户端不存在")
}

// 发送在线用户列表
func (p *ConnectionPool) sendUserList(conn *websocket.Conn) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	users := make([]string, 0, len(p.clients))
	for _, client := range p.clients {
		users = append(users, fmt.Sprintf("%s (%s)", client.Name, client.ID))
	}

	message := Message{
		Type:    "user_list",
		Content: users,
		Time:    time.Now().Format("2006-01-02 15:04:05"),
		From:    "系统",
	}

	conn.WriteJSON(message)
}

// 获取在线用户数量
func (p *ConnectionPool) getOnlineCount() int {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return len(p.clients)
}

// 服务器端处理函数（使用连接池）
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	// 生成客户端ID和名称
	clientID := fmt.Sprintf("client_%d", time.Now().UnixNano())
	clientName := r.URL.Query().Get("name")
	if clientName == "" {
		clientName = fmt.Sprintf("玩家%d", pool.getOnlineCount()+1)
	}

	// 创建客户端信息
	client := &Client{
		ID:       clientID,
		Name:     clientName,
		Conn:     conn,
		Addr:     conn.RemoteAddr().String(),
		JoinTime: time.Now(),
	}

	// 注册客户端到连接池
	pool.register <- client

	defer func() {
		// 注销客户端
		pool.unregister <- conn
	}()

	// 发送欢迎消息
	welcome := Message{
		Type:    "welcome",
		Content: fmt.Sprintf("欢迎 %s 连接到游戏服务器！", clientName),
		Time:    time.Now().Format("2006-01-02 15:04:05"),
		From:    "系统",
	}
	conn.WriteJSON(welcome)

	// 处理客户端消息
	for {
		var msg map[string]interface{}
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Printf("读取消息错误 (%s): %v", clientName, err)
			break
		}

		log.Printf("收到消息 from %s: %+v", clientName, msg)

		// 处理不同类型的消息
		switch msg["type"] {
		case "ping":
			response := Message{
				Type:    "pong",
				Content: "服务器响应: pong",
				Time:    time.Now().Format("2006-01-02 15:04:05"),
				From:    "服务器",
			}
			conn.WriteJSON(response)

		case "chat":
			chatContent := msg["content"]
			if chatContent == nil {
				chatContent = "空消息"
			}

			// 广播聊天消息给所有客户端
			chatMessage := Message{
				Type:    "chat",
				Content: fmt.Sprintf("%s: %v", clientName, chatContent),
				Time:    time.Now().Format("2006-01-02 15:04:05"),
				From:    clientName,
			}
			pool.broadcastMessage(chatMessage)

		case "move":
			// 广播移动消息
			moveMessage := Message{
				Type:    "move",
				Content: fmt.Sprintf("%s 移动到了 x=%v, y=%v", clientName, msg["x"], msg["y"]),
				Time:    time.Now().Format("2006-01-02 15:04:05"),
				From:    clientName,
			}
			pool.broadcastMessage(moveMessage)

		case "whisper":
			// 私聊功能（发送给特定用户）
			target := msg["target"]
			content := msg["content"]
			if target != nil && content != nil {
				// 这里可以扩展为根据用户名查找连接
				log.Printf("%s 想私聊给 %s: %v", clientName, target, content)
			}

		default:
			response := Message{
				Type:    "echo",
				Content: fmt.Sprintf("服务器回显: %+v", msg),
				Time:    time.Now().Format("2006-01-02 15:04:05"),
				From:    "服务器",
			}
			conn.WriteJSON(response)
		}
	}

	log.Printf("客户端 %s 断开连接", clientName)
}

// 服务器初始化
func WebsocketServerInit(port string) {
	http.HandleFunc("/ws", handleWebSocket)

	log.Printf("游戏WebSocket服务器启动在 %s", port)
	log.Printf("WebSocket端点: ws://localhost%s/ws", port)
	log.Println("支持功能:")
	log.Println("  - 多用户连接管理")
	log.Println("  - 消息广播")
	log.Println("  - 在线用户列表")
	log.Println("  - 用户加入/离开通知")
	log.Fatal(http.ListenAndServe(port, nil))
}
