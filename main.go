package main

import (
	"fmt"
	"log"
	"os"
	"server/websocketserver"

	"github.com/alecthomas/kingpin/v2"
)

var (
	app = kingpin.New("gameapp", "多人在线游戏应用")

	// 服务端命令
	serverCmd   = app.Command("server", "启动游戏服务器")
	serverPort  = serverCmd.Flag("port", "服务器端口").Default("8081").Short('p').String()
	serverHost  = serverCmd.Flag("host", "服务器地址").Default("0.0.0.0").Short('H').String()
	serverDebug = serverCmd.Flag("debug", "启用调试模式").Bool()

	// 客户端命令
	clientCmd    = app.Command("client", "启动游戏客户端")
	clientServer = clientCmd.Flag("server", "连接的服务端地址").Default("localhost:8081").Short('s').String()
	clientName   = clientCmd.Flag("name", "客户端名称").Default("Player1").Short('n').String()
	clientAuto   = clientCmd.Flag("auto", "自动发送测试消息").Bool()

	// 通用参数（可在任何命令中使用）
	verbose = app.Flag("verbose", "详细输出").Short('v').Bool()
)

func main() {
	app.Version("1.0.0")
	app.Author("游戏开发团队")
	app.HelpFlag.Short('h') // 设置 -h 为帮助

	// 解析命令行参数
	command := kingpin.MustParse(app.Parse(os.Args[1:]))

	// 根据命令执行相应逻辑
	switch command {
	case serverCmd.FullCommand():
		startServer()
	case clientCmd.FullCommand():
		startClient()
	}
}

func startServer() {
	// 构建服务器地址
	addr := fmt.Sprintf("%s:%s", *serverHost, *serverPort)
	log.Printf("starting server mode on %s", addr)

	if *serverDebug {
		log.Println("调试模式已启用")
	}

	// 传递参数给WebsocketServerInit
	websocketserver.WebsocketServerInit(addr)
}

func startClient() {
	log.Printf("starting client mode")
	log.Printf("连接服务器: %s", *clientServer)
	log.Printf("客户端名称: %s", *clientName)

	if *clientAuto {
		log.Println("自动发送模式已启用")
	}

	// 调用客户端初始化
	websocketserver.WebSocketClientInit(*clientServer, *clientName, *clientAuto)
}
