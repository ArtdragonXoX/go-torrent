package torrent

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

type TorrentTask struct {
	PeerId       [20]byte
	PeerList     []PeerInfo
	InfoSHA      [SHALEN]byte
	FileName     string
	FileLen      int
	PieceLen     int
	PieceSHA     [][SHALEN]byte
	DHT          *DHT
	peerMu       sync.RWMutex
	peerFailures map[string]int // 记录每个peer的失败次数
	failureMu    sync.RWMutex   // 保护peerFailures的互斥锁
}

type pieceTask struct {
	index  int
	sha    [SHALEN]byte
	length int
}

type taskState struct {
	index      int
	conn       *PeerConn
	requested  int
	downloaded int
	backlog    int
	data       []byte
}

type pieceResult struct {
	index int
	data  []byte
}

const (
	BLOCKSIZE          = 16384
	MAXBACKLOG         = 5
	DHT_QUERY_INTERVAL = 10 * time.Second
)

func (t *TorrentTask) addPeers(peers []PeerInfo) {
	t.peerMu.Lock()
	defer t.peerMu.Unlock()

	existing := make(map[string]struct{})
	for _, p := range t.PeerList {
		key := fmt.Sprintf("%s:%d", p.Ip.String(), p.Port)
		existing[key] = struct{}{}
	}

	// 检查失败次数
	t.failureMu.RLock()
	for _, p := range peers {
		key := fmt.Sprintf("%s:%d", p.Ip.String(), p.Port)
		// 检查是否已存在或已达到最大失败次数
		if _, ok := existing[key]; !ok {
			// 检查失败次数
			failures, exists := t.peerFailures[key]
			if !exists || failures < 5 {
				t.PeerList = append(t.PeerList, p)
			} else {
				fmt.Printf("跳过添加已达到最大失败次数的Peer %s\n", key)
			}
		}
	}
	t.failureMu.RUnlock()
}

func (t *TorrentTask) startDHTQuery() {
	if t.DHT == nil {
		return
	}

	ticker := time.NewTicker(DHT_QUERY_INTERVAL)
	defer ticker.Stop()
	fmt.Println("开始查询DHT")

	for {
		select {
		case <-ticker.C:
			peers := t.DHT.GetPeers(t.InfoSHA)
			if len(peers) > 0 {
				t.addPeers(peers)
			}
		}
	}
}

func (state *taskState) handleMsg() error {
	msg, err := state.conn.ReadMsg()
	if err != nil {
		return err
	}
	if msg == nil {
		return nil
	}
	switch msg.Id {
	case MsgChoke:
		state.conn.Choked = true
	case MsgUnchoke:
		state.conn.Choked = false
	case MsgHave:
		index, err := GetHaveIndex(msg)
		if err != nil {
			return err
		}
		state.conn.Field.SetPiece(index)
	case MsgPiece:
		n, err := CopyPieceData(state.index, state.data, msg)
		if err != nil {
			return err
		}
		state.downloaded += n
		state.backlog--
	case MsgDHTNodes:
		// 使用专门的函数处理DHT节点交换消息
		handleDHTNodesMsg(state.conn, msg)
	}
	return nil
}

// 处理接收到的DHT节点消息
func handleDHTNodesMsg(conn *PeerConn, msg *PeerMsg) {
	// 如果是空消息，可能是节点请求，已在ReadMsg中处理
	if conn.task == nil || conn.task.DHT == nil || len(msg.Payload) == 0 {
		return
	}

	// 解码接收到的DHT节点信息
	nodes := DecodeNodes(msg.Payload)
	if len(nodes) > 0 {
		// 将这些节点添加到DHT网络
		conn.task.DHT.AddNodesFromPeer(nodes)
		peerKey := fmt.Sprintf("%s:%d", conn.peer.Ip.String(), conn.peer.Port)
		fmt.Printf("从Peer %s 接收到%d个DHT节点\n", peerKey, len(nodes))

		// 如果DHT节点数量仍然较少，可以主动回复我们的节点
		conn.task.DHT.nodesMu.RLock()
		nodeCount := len(conn.task.DHT.nodes)
		conn.task.DHT.nodesMu.RUnlock()

		if nodeCount < 50 { // 如果节点数量仍然不多，回复我们的节点
			// 编码本地DHT节点并发送给peer
			nodesData := conn.task.DHT.EncodeNodes()
			if len(nodesData) > 0 {
				dhtMsg := NewDHTNodesMsg(nodesData)
				_, err := conn.WriteMsg(dhtMsg)
				if err != nil {
					fmt.Printf("向Peer %s 回复DHT节点信息失败: %v\n", peerKey, err)
				} else {
					fmt.Printf("向Peer %s 回复了DHT节点信息（互换）\n", peerKey)
				}
			}
		}
	}
}

func downloadPiece(conn *PeerConn, task *pieceTask) (*pieceResult, error) {
	state := &taskState{
		index: task.index,
		conn:  conn,
		data:  make([]byte, task.length),
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetDeadline(time.Time{})

	for state.downloaded < task.length {
		if !conn.Choked {
			for state.backlog < MAXBACKLOG && state.requested < task.length {
				length := BLOCKSIZE
				if task.length-state.requested < length {
					length = task.length - state.requested
				}
				msg := NewRequestMsg(state.index, state.requested, length)
				_, err := state.conn.WriteMsg(msg)
				if err != nil {
					return nil, err
				}
				state.backlog++
				state.requested += length
			}
		}
		err := state.handleMsg()
		if err != nil {
			return nil, err
		}
	}
	return &pieceResult{state.index, state.data}, nil
}

func checkPiece(task *pieceTask, res *pieceResult) bool {
	sha := sha1.Sum(res.data)
	if !bytes.Equal(task.sha[:], sha[:]) {
		fmt.Printf("check integrity failed, index :%v\n", res.index)
		return false
	}
	return true
}

// peerRoutine 处理与单个peer的通信，包含失败处理机制
func (t *TorrentTask) peerRoutine(peer PeerInfo, taskQueue chan *pieceTask, resultQueue chan *pieceResult) {
	// 生成peer的唯一标识
	peerKey := fmt.Sprintf("%s:%d", peer.Ip.String(), peer.Port)

	// 检查此peer是否已经达到最大失败次数
	t.failureMu.RLock()
	failures, exists := t.peerFailures[peerKey]
	t.failureMu.RUnlock()

	if exists && failures >= 5 {
		// 不仅跳过，还要确保从PeerList中移除
		t.removePeer(peerKey)
		fmt.Printf("Peer %s 已达到最大失败次数，已移除\n", peerKey)
		return
	}

	conn, err := NewConn(peer, t.InfoSHA, t.PeerId, t)
	if err != nil {
		// 连接失败，增加失败计数
		t.incrementFailure(peerKey)
		return
	}
	// 注册活跃连接
	registerActivePeer(peerKey, conn)
	defer func() {
		// 注销活跃连接
		unregisterActivePeer(peerKey)
		conn.Close()
	}()

	// 发送interested消息
	conn.WriteMsg(&PeerMsg{MsgInterested, nil})

	// 如果DHT可用，尝试交换DHT节点信息
	if t.DHT != nil {
		// 编码本地DHT节点并发送给peer
		nodesData := t.DHT.EncodeNodes()
		if len(nodesData) > 0 {
			dhtMsg := NewDHTNodesMsg(nodesData)
			_, err := conn.WriteMsg(dhtMsg)
			if err != nil {
				fmt.Printf("向Peer %s 发送DHT节点信息失败: %v\n", peerKey, err)
			} else {
				fmt.Printf("向Peer %s 发送了DHT节点信息\n", peerKey)
			}
		}
	}

	failureCount := 0 // 当前会话中的连续失败次数

	// 创建一个goroutine来处理来自peer的消息
	go func() {
		for {
			// 设置较短的超时时间，以便能够及时处理其他消息
			conn.SetDeadline(time.Now().Add(1 * time.Second))
			msg, err := conn.ReadMsg()
			if err != nil {
				// 忽略超时错误
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				// 其他错误则退出
				break
			}

			if msg == nil {
				continue
			}

			// 处理DHT节点消息
			if msg.Id == MsgDHTNodes {
				handleDHTNodesMsg(conn, msg)
			}
		}
	}()

	for task := range taskQueue {
		// 检查是否已经达到最大失败次数
		if failureCount >= 5 {
			t.removePeer(peerKey)
			fmt.Printf("Peer %s 在当前会话中连续失败5次，已移除\n", peerKey)
			return
		}

		if !conn.Field.HasPiece(task.index) {
			taskQueue <- task
			continue
		}

		res, err := downloadPiece(conn, task)
		if err != nil {
			failureCount++
			t.incrementFailure(peerKey)
			fmt.Printf("从Peer %s 下载失败: %v, 失败计数: %d\n", peerKey, err, failureCount)
			taskQueue <- task
			continue
		}

		if !checkPiece(task, res) {
			failureCount++
			t.incrementFailure(peerKey)
			fmt.Printf("从Peer %s 下载的数据校验失败, 失败计数: %d\n", peerKey, failureCount)
			taskQueue <- task
			continue
		}

		// 成功下载，重置失败计数
		failureCount = 0
		resultQueue <- res
	}
}

func (t *TorrentTask) getPieceBounds(index int) (begin, end int) {
	begin = index * t.PieceLen
	end = begin + t.PieceLen
	if end > t.FileLen {
		end = t.FileLen
	}
	return
}

// incrementFailure 增加指定peer的失败计数
func (t *TorrentTask) incrementFailure(peerKey string) {
	t.failureMu.Lock()
	defer t.failureMu.Unlock()

	// 初始化map（如果需要）
	if t.peerFailures == nil {
		t.peerFailures = make(map[string]int)
	}

	// 增加失败计数
	t.peerFailures[peerKey]++
	fmt.Printf("Peer %s 失败次数增加到 %d\n", peerKey, t.peerFailures[peerKey])
}

// removePeer 从PeerList中移除指定的peer
func (t *TorrentTask) removePeer(peerKey string) {
	t.peerMu.Lock()
	defer t.peerMu.Unlock()

	// 记录此peer已达到最大失败次数
	t.failureMu.Lock()
	t.peerFailures[peerKey] = 5 // 设置为最大失败次数
	t.failureMu.Unlock()

	// 从PeerList中移除
	var newPeerList []PeerInfo
	for _, p := range t.PeerList {
		currentKey := fmt.Sprintf("%s:%d", p.Ip.String(), p.Port)
		if currentKey != peerKey {
			newPeerList = append(newPeerList, p)
		}
	}

	t.PeerList = newPeerList
	fmt.Printf("已从PeerList中移除Peer %s\n", peerKey)
}

// cleanFailedPeers 清理已达到最大失败次数的peer
func (t *TorrentTask) cleanFailedPeers() {
	t.failureMu.RLock()
	var failedPeers []string
	for peerKey, failures := range t.peerFailures {
		if failures >= 5 {
			failedPeers = append(failedPeers, peerKey)
		}
	}
	t.failureMu.RUnlock()

	if len(failedPeers) > 0 {
		t.peerMu.Lock()
		var newPeerList []PeerInfo
		for _, p := range t.PeerList {
			currentKey := fmt.Sprintf("%s:%d", p.Ip.String(), p.Port)
			skip := false
			for _, failedKey := range failedPeers {
				if currentKey == failedKey {
					skip = true
					break
				}
			}
			if !skip {
				newPeerList = append(newPeerList, p)
			}
		}

		if len(t.PeerList) != len(newPeerList) {
			fmt.Printf("已从PeerList中清理 %d 个失败次数过多的Peer\n", len(t.PeerList)-len(newPeerList))
			t.PeerList = newPeerList
		}
		t.peerMu.Unlock()
	}
}

// 用于存储活跃的peer连接
type activePeerConn struct {
	conn    *PeerConn
	peerKey string
}

// 活跃连接管理
var (
	activePeers   = make(map[string]*PeerConn)
	activePeersMu sync.RWMutex
)

// registerActivePeer 注册一个活跃的peer连接
func registerActivePeer(peerKey string, conn *PeerConn) {
	activePeersMu.Lock()
	defer activePeersMu.Unlock()
	activePeers[peerKey] = conn
}

// unregisterActivePeer 注销一个活跃的peer连接
func unregisterActivePeer(peerKey string) {
	activePeersMu.Lock()
	defer activePeersMu.Unlock()
	delete(activePeers, peerKey)
}

// getActivePeers 获取当前所有活跃的peer连接
func getActivePeers() []*activePeerConn {
	activePeersMu.RLock()
	defer activePeersMu.RUnlock()

	var result []*activePeerConn
	for key, conn := range activePeers {
		result = append(result, &activePeerConn{conn: conn, peerKey: key})
	}
	return result
}

// requestDHTNodesFromPeers 向所有活跃的peer请求DHT节点
func (t *TorrentTask) requestDHTNodesFromPeers() {
	if t.DHT == nil {
		return
	}

	// 获取所有活跃的peer连接
	activePeers := getActivePeers()
	if len(activePeers) == 0 {
		fmt.Println("没有活跃的peer连接，无法请求DHT节点")
		return
	}

	fmt.Printf("开始向%d个活跃peer请求DHT节点\n", len(activePeers))

	// 创建一个空的DHT节点请求消息
	// 这是一个特殊的消息，告诉peer我们需要DHT节点
	dhtRequestMsg := NewDHTNodesMsg(nil)

	// 向所有活跃的peer发送请求
	for _, peer := range activePeers {
		if peer.conn != nil {
			// 设置较短的超时时间
			peer.conn.SetDeadline(time.Now().Add(2 * time.Second))

			// 发送DHT节点请求
			_, err := peer.conn.WriteMsg(dhtRequestMsg)
			if err != nil {
				fmt.Printf("向Peer %s 请求DHT节点失败: %v\n", peer.peerKey, err)
			} else {
				fmt.Printf("向Peer %s 发送了DHT节点请求\n", peer.peerKey)
			}

			// 重置超时时间
			peer.conn.SetDeadline(time.Time{})
		}
	}
}

// Download 下载种子文件，如果所有peer都不可用则终止任务并返回错误
func Download(task *TorrentTask) error {
	var err error
	fmt.Println("start downloading " + task.FileName)

	// 初始化失败计数map
	task.peerFailures = make(map[string]int)

	// 清理已达到最大失败次数的peer
	task.cleanFailedPeers()

	// 检查是否有可用的peer
	task.peerMu.RLock()
	if len(task.PeerList) == 0 {
		task.peerMu.RUnlock()
		return fmt.Errorf("没有可用的peer，下载任务终止")
	}
	task.peerMu.RUnlock()

	taskQueue := make(chan *pieceTask, len(task.PieceSHA))
	resultQueue := make(chan *pieceResult)

	for index, sha := range task.PieceSHA {
		begin, end := task.getPieceBounds(index)
		taskQueue <- &pieceTask{index, sha, (end - begin)}
	}

	// 初始化DHT网络，使用默认端口6881
	task.DHT, err = NewDHT(6881)
	if err != nil {
		fmt.Printf("DHT初始化失败: %v\n", err)
		task.DHT = nil
	}

	// Start DHT query goroutine
	if task.DHT != nil {
		go task.startDHTQuery()

		// 启动一个额外的goroutine，定期检查是否有新的DHT节点从peer获取
		go func() {
			dhtNodesTicker := time.NewTicker(30 * time.Second)
			defer dhtNodesTicker.Stop()

			for {
				select {
				case <-dhtNodesTicker.C:
					// 检查DHT网络中的节点数量
					task.DHT.nodesMu.RLock()
					nodeCount := len(task.DHT.nodes)
					task.DHT.nodesMu.RUnlock()

					fmt.Printf("当前DHT网络中有%d个节点\n", nodeCount)

					// 如果节点数量较少，主动向所有已连接的peer请求DHT节点
					if nodeCount < 20 {
						fmt.Println("DHT节点数量较少，尝试从已连接的peer获取更多节点")
						// 请求DHT节点
						task.requestDHTNodesFromPeers()
					}
				}
			}
		}()
	}

	// Start initial peers
	for _, peer := range task.PeerList {
		go task.peerRoutine(peer, taskQueue, resultQueue)
	}

	// 创建定期清理失败peer的计时器
	cleanupTicker := time.NewTicker(30 * time.Second)
	defer cleanupTicker.Stop()

	// 创建一个用于检查peer列表是否为空的计时器
	peerCheckTicker := time.NewTicker(15 * time.Second)
	defer peerCheckTicker.Stop()

	buf := make([]byte, task.FileLen)
	count := 0
	for count < len(task.PieceSHA) {
		select {
		case res := <-resultQueue:
			begin, end := task.getPieceBounds(res.index)
			copy(buf[begin:end], res.data)
			count++

			percent := float64(count) / float64(len(task.PieceSHA)) * 100
			fmt.Printf("downloading, progress : (%0.2f%%)\n", percent)
		case <-cleanupTicker.C:
			// 定期清理失败次数过多的peer
			task.cleanFailedPeers()

			// 清理后检查peer列表是否为空
			task.peerMu.RLock()
			if len(task.PeerList) == 0 {
				task.peerMu.RUnlock()
				return fmt.Errorf("所有peer都不可用，下载任务终止")
			}
			task.peerMu.RUnlock()
		case <-peerCheckTicker.C:
			// 定期检查peer列表是否为空
			task.peerMu.RLock()
			if len(task.PeerList) == 0 {
				task.peerMu.RUnlock()
				return fmt.Errorf("所有peer都不可用，下载任务终止")
			}
			task.peerMu.RUnlock()
		}

		// Add new peers periodically
		task.peerMu.RLock()
		newPeers := task.PeerList
		task.peerMu.RUnlock()

		for _, peer := range newPeers {
			go task.peerRoutine(peer, taskQueue, resultQueue)
		}
	}

	close(taskQueue)
	close(resultQueue)

	file, err := os.Create(task.FileName)
	if err != nil {
		return err
	}
	_, err = file.Write(buf)
	return err
}
