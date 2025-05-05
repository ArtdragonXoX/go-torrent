package main

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"os"

	"bt_download/torrent"
)

func main() {
	// 检查命令行参数
	if len(os.Args) < 2 {
		fmt.Println("用法: main.exe <torrent文件路径> [-log]")
		return
	}
	// 确保在程序退出前关闭日志文件
	defer torrent.CloseLogger()

	// 解析命令行参数
	torrentFilePath := os.Args[1]

	//查是否有-log参数
	enableFileLogging := false
	for i := 2; i < len(os.Args); i++ {
		if os.Args[i] == "-log" {
			enableFileLogging = true
			break
		}
	}

	// 初始化日志配置
	torrent.InitLogger(enableFileLogging)

	//parse torrent file
	file, err := os.Open(torrentFilePath)
	if err != nil {
		fmt.Println("open file error")
		return
	}
	defer file.Close()
	tf, err := torrent.ParseFile(bufio.NewReader(file))
	if err != nil {
		fmt.Println("parse file error")
		return
	}
	// random peerId
	var peerId [torrent.IDLEN]byte
	_, _ = rand.Read(peerId[:])
	// build peer manager
	torrent.Download(peerId, tf)
}
