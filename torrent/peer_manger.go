package torrent

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

const ERROR_COUNT = 5

type PeerManager struct {
	peerId      [20]byte
	tf          *TorrentFile
	peers       map[string]PeerWithStatus
	mu          sync.Mutex
	task        *TorrentTask
	taskQueue   chan *pieceTask
	resultQueue chan *pieceResult
	// 用于管理每个peer的context
	peerCtx    map[string]context.Context
	peerCancel map[string]context.CancelFunc
	ctxMu      sync.Mutex // 保护context相关map的互斥锁
}

func NewPeerManager(peerId [20]byte, tf *TorrentFile, resultQueue chan *pieceResult) *PeerManager {
	task := &TorrentTask{
		InfoSHA:  tf.InfoSHA,
		FileName: tf.FileName,
		FileLen:  tf.FileLen,
		PieceLen: tf.PieceLen,
		PieceSHA: tf.PieceSHA,
	}
	return &PeerManager{
		peerId:      peerId,
		tf:          tf,
		task:        task,
		peers:       make(map[string]PeerWithStatus),
		peerCtx:     make(map[string]context.Context),
		peerCancel:  make(map[string]context.CancelFunc),
		resultQueue: resultQueue,
	}
}

func (pm *PeerManager) AddPeer(peer PeerInfo) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	_, ok := pm.peers[peer.Ip.String()]
	if ok {
		return
	}

	pm.peers[peer.Ip.String()] = PeerWithStatus{
		PeerInfo:   peer,
		ErrorCount: 0,
	}
}

func (pm *PeerManager) RemovePeer(peer PeerInfo) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	peerKey := peer.Ip.String()
	delete(pm.peers, peerKey)

	// 取消对应peer的context
	pm.ctxMu.Lock()
	defer pm.ctxMu.Unlock()
	if cancel, ok := pm.peerCancel[peerKey]; ok {
		cancel() // 发送取消信号
		delete(pm.peerCtx, peerKey)
		delete(pm.peerCancel, peerKey)
		logger.Info("终止peer连接", zap.String("peer", peerKey))
		fmt.Println("终止peer连接 : " + peerKey)
		fmt.Println("peer数量 : " + strconv.Itoa(len(pm.peers)))
	}
}

func (pm *PeerManager) reportError(peer PeerInfo) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	peerKey := peer.Ip.String()
	peerWithStatus, ok := pm.peers[peerKey]
	if !ok {
		return
	}

	peerWithStatus.ErrorCount++
	pm.peers[peerKey] = peerWithStatus
	fmt.Printf("peer %v 错误次数 : %v\n", peerKey, peerWithStatus.ErrorCount)
	if peerWithStatus.ErrorCount > ERROR_COUNT {
		// 解锁以避免死锁，因为RemovePeer也会获取锁
		pm.mu.Unlock()
		pm.RemovePeer(peer)
		pm.mu.Lock()
	}
}

func (pm *PeerManager) peerRoutine(ctx context.Context, peer PeerInfo, taskQueue chan *pieceTask, resultQueue chan *pieceResult) {
	conn, err := NewConn(peer, pm.tf.InfoSHA, pm.peerId)
	if err != nil {
		logger.Warn("连接peer失败 : " + peer.Ip.String())
		pm.reportError(peer)
		return
	}
	defer conn.Close()

	logger.Info("成功与peer握手 : " + peer.Ip.String())
	conn.WriteMsg(&PeerMsg{MsgInterested, nil})
	for {
		select {
		case <-ctx.Done():
			// context被取消，终止goroutine
			logger.Info("peer goroutine被终止", zap.String("peer", peer.Ip.String()))
			return
		case task, ok := <-taskQueue:
			if !ok {
				// taskQueue已关闭
				return
			}

			if !conn.Field.HasPiece(task.index) {
				taskQueue <- task
				continue
			}
			logger.Info(fmt.Sprintf("获取到任务, index: %v, peer : %v", task.index, peer.Ip.String()))
			res, err := downloadPiece(conn, task)
			if err != nil {
				taskQueue <- task
				logger.Warn("下载piece失败" + err.Error())
				pm.reportError(peer)
				continue
			}
			if !checkPiece(task, res) {
				taskQueue <- task
				logger.Warn("下载piece失败, 校验失败")
				pm.reportError(peer)
				continue
			}
			resultQueue <- res
		}
	}
}

// safeSendResult 安全地向channel发送数据，确保context未超时
// 返回值表示是否成功发送
func safeSendResult(ctx context.Context, ch chan<- TrackerResult, result TrackerResult) bool {
	select {
	case <-ctx.Done():
		return false
	case ch <- result:
		return true
	}
}

func (pm *PeerManager) updatePeerFromTracker() {
	trackers := []string{pm.tf.Announce}
	backupTrackers := readBackupTrackers(".\\tracker.txt")
	if backupTrackers != nil {
		trackers = append(trackers, backupTrackers...)
	}
	logger.Info("开始从Tracker获取Peer",
		zap.Int("tracker总数", len(trackers)),
		zap.String("种子名称", pm.tf.FileName))

	// 创建带超时的context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resultChan := make(chan TrackerResult, len(trackers))
	defer close(resultChan)

	for _, tracker := range trackers {
		go func(tracker string) {
			// 检查context是否已超时
			select {
			case <-ctx.Done():
				logger.Warn("Tracker请求超时",
					zap.String("tracker", tracker))
				return
			default:
			}
			// 将[20]byte类型转换为[]byte和string类型
			infoHashSlice := pm.tf.InfoSHA[:] // 将[20]byte转换为[]byte
			peerIdStr := string(pm.peerId[:]) // 将[20]byte转换为string
			resp, err := AnnounceToTracker(tracker, infoHashSlice, peerIdStr, PeerPort)
			if err != nil || resp == nil {
				logger.Warn("Tracker请求失败",
					zap.String("tracker", tracker),
					zap.Error(err))
				// 检查context是否已超时
				select {
				case <-ctx.Done():
					logger.Warn("Tracker请求超时",
						zap.String("tracker", tracker))
					return
				default:
				}
				if !safeSendResult(ctx, resultChan, TrackerResult{URL: tracker, Peers: nil}) {
					return
				}
				return
			}
			// 解析响应获取Peer列表
			peers, err := ParsePeersFromResponse(resp)
			if err != nil {
				logger.Warn("解析Tracker响应失败",
					zap.String("tracker", tracker),
					zap.Error(err))
				// 检查context是否已超时
				select {
				case <-ctx.Done():
					logger.Warn("Tracker请求超时",
						zap.String("tracker", tracker))
					return
				default:
				}
				if !safeSendResult(ctx, resultChan, TrackerResult{URL: tracker, Peers: nil}) {
					return
				}
				return
			}
			logger.Info("Tracker响应成功",
				zap.String("tracker", tracker),
				zap.Int("peers数量", len(peers)))
			// 检查context是否已超时
			select {
			case <-ctx.Done():
				logger.Warn("Tracker请求超时",
					zap.String("tracker", tracker))
				return
			default:
			}
			resultChan <- TrackerResult{URL: tracker, Peers: peers}
		}(tracker)
	}

	// 从resultChan中收集结果
	for i := 0; i < len(trackers); i++ {
		select {
		case <-ctx.Done():
			logger.Warn("收集Peer结果超时")
			return
		case result := <-resultChan:
			if result.Peers != nil {
				for _, peer := range result.Peers {
					logger.Info("获取到peer",
						zap.String("tracker", result.URL),
						zap.String("peer", peer.Ip.String()),
						zap.Int("port", int(peer.Port)))
					pm.AddPeer(peer)
					// 为新添加的peer启动下载任务处理goroutine
					if pm.taskQueue != nil && pm.resultQueue != nil {
						pm.StartPeerRoutine(peer)
					}
				}
			}
		}
	}
}

// InitTaskQueues 初始化任务队列
// 在启动peer连接前必须调用此方法
func (pm *PeerManager) InitTaskQueues() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.taskQueue = make(chan *pieceTask, len(pm.task.PieceSHA))
	for index, sha := range pm.task.PieceSHA {
		begin, end := pm.task.getPieceBounds(index)
		pm.taskQueue <- &pieceTask{index, sha, (end - begin)}
	}
	logger.Info("初始化任务队列完成")
}

// StartPeerRoutine 启动与peer的连接并处理下载任务
// 为每个peer创建独立的context，当peer错误达到阈值时可以终止对应的goroutine
func (pm *PeerManager) StartPeerRoutine(peer PeerInfo) {
	pm.ctxMu.Lock()
	peerKey := peer.Ip.String()

	// 检查是否已经为该peer启动了goroutine
	_, exists := pm.peerCtx[peerKey]
	if exists {
		pm.ctxMu.Unlock()
		return
	}

	// 为peer创建新的context
	ctx, cancel := context.WithCancel(context.Background())
	pm.peerCtx[peerKey] = ctx
	pm.peerCancel[peerKey] = cancel
	pm.ctxMu.Unlock()

	// 启动goroutine处理下载任务
	go func() {
		pm.peerRoutine(ctx, peer, pm.taskQueue, pm.resultQueue)

		// goroutine结束时清理context
		pm.ctxMu.Lock()
		delete(pm.peerCtx, peerKey)
		delete(pm.peerCancel, peerKey)
		pm.ctxMu.Unlock()
	}()

	logger.Info("启动peer连接", zap.String("peer", peerKey))
}

// StartAllPeers 启动所有已添加的peers的连接
// 在初始化任务队列后调用此方法可以启动所有现有peer的下载任务
func (pm *PeerManager) StartAllPeers() {
	// 确保任务队列已初始化
	if pm.taskQueue == nil || pm.resultQueue == nil {
		logger.Warn("任务队列未初始化，无法启动peer连接")
		return
	}

	pm.mu.Lock()
	peers := make([]PeerInfo, 0, len(pm.peers))
	for _, peerWithStatus := range pm.peers {
		peers = append(peers, peerWithStatus.PeerInfo)
	}
	pm.mu.Unlock()

	logger.Info("开始启动所有peer连接", zap.Int("peer数量", len(peers)))
	for _, peer := range peers {
		pm.StartPeerRoutine(peer)
	}
}

// StopAllPeers 停止所有peer连接
// 在需要终止所有下载任务时调用此方法
func (pm *PeerManager) StopAllPeers() {
	pm.ctxMu.Lock()
	defer pm.ctxMu.Unlock()

	// 取消所有peer的context
	for peerKey, cancel := range pm.peerCancel {
		cancel()
		logger.Info("终止peer连接", zap.String("peer", peerKey))
	}

	// 清空context和cancel映射
	pm.peerCtx = make(map[string]context.Context)
	pm.peerCancel = make(map[string]context.CancelFunc)

	logger.Info("已停止所有peer连接")
}

// Start 启动PeerManager，开始从Tracker获取Peer，并启动所有已添加的peers的连接
func (pm *PeerManager) Start() {
	// 启动定时更新Peer的goroutine
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		pm.updatePeerFromTracker()
		for {
			fmt.Printf("peer数量 : %v\n", len(pm.peers))
			select {
			case <-ticker.C:
				fmt.Println("定时更新peer")
				pm.updatePeerFromTracker()
			}
		}
	}()

	pm.InitTaskQueues() // 初始化任务队列
	pm.StartAllPeers()  // 启动所有已添加的peers连接
}
