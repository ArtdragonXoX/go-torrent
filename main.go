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
		fmt.Println("用法: main.exe <torrent文件路径>")
		return
	}
	//parse torrent file
	file, err := os.Open(os.Args[1])
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
	//connect tracker & find peers
	peers := torrent.FindPeers(tf, peerId)
	if len(peers) == 0 {
		fmt.Println("can not find peers")
		return
	}
	// build torrent task
	task := &torrent.TorrentTask{
		PeerId:   peerId,
		PeerList: peers,
		InfoSHA:  tf.InfoSHA,
		FileName: tf.FileName,
		FileLen:  tf.FileLen,
		PieceLen: tf.PieceLen,
		PieceSHA: tf.PieceSHA,
	}
	//download from peers & make file
	torrent.Download(task)
}
