package torrent

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"bt_download/bencode"
)

const (
	PeerPort int = 6666
	IpLen    int = 4
	PortLen  int = 2
	PeerLen  int = IpLen + PortLen
)

const IDLEN int = 20

type PeerInfo struct {
	Ip       net.IP
	Port     uint16
	LastSeen time.Time
}

type TrackerResp struct {
	Interval       int    `bencode:"interval"`
	Peers          string `bencode:"peers"`
	Complete       int    `bencode:"complete"`
	Incomplete     int    `bencode:"incomplete"`
	MinInterval    int    `bencode:"min interval"`
	FailureReason  string `bencode:"failure reason"`
	WarningMessage string `bencode:"warning message"`
}

// buildUrl 构建Tracker HTTP请求URL
// 特别注意info_hash和peer_id需要正确编码
func buildUrl(tf *TorrentFile, peerId [IDLEN]byte) (string, error) {
	// 解析基础URL
	base, err := url.Parse(tf.Announce)
	if err != nil {
		fmt.Printf("[ERROR] 解析Announce URL失败: %s, 错误: %s\n", tf.Announce, err.Error())
		return "", err
	}

	// 创建查询参数
	params := url.Values{}

	// 手动添加info_hash (需要特殊处理，不能使用标准URL编码)
	// info_hash必须是原始的20字节，而不是十六进制字符串
	ihash := make([]byte, SHALEN)
	copy(ihash, tf.InfoSHA[:])
	params.Set("info_hash", string(ihash))

	// 手动添加peer_id (同样需要特殊处理)
	pid := make([]byte, IDLEN)
	copy(pid, peerId[:])
	params.Set("peer_id", string(pid))

	// 添加其他参数
	params.Set("port", strconv.Itoa(PeerPort))
	params.Set("uploaded", "0")
	params.Set("downloaded", "0")
	params.Set("compact", "1") // 请求紧凑的peer列表格式
	params.Set("left", strconv.Itoa(tf.FileLen))

	// 添加event=started参数，表示这是初始请求
	params.Set("event", "started")

	// 添加numwant参数，请求更多peer
	params.Set("numwant", "100")

	// 设置查询字符串
	base.RawQuery = params.Encode()

	// 记录构建的URL (不包含敏感的二进制数据)
	fmt.Printf("[DEBUG] 构建Tracker URL: %s (参数已省略敏感数据)\n", base.String())

	return base.String(), nil
}

// buildPeerInfo 解析二进制格式的peer信息列表
// 每个peer占用6字节，前4字节为IP地址，后2字节为端口
// 返回解析后的PeerInfo结构体切片
func buildPeerInfo(peers []byte) []PeerInfo {
	// 检查输入数据
	if len(peers) == 0 {
		fmt.Printf("[ERROR] 收到空的peers数据\n")
		return nil
	}

	num := len(peers) / PeerLen
	if len(peers)%PeerLen != 0 {
		fmt.Printf("[ERROR] 收到格式不正确的peers数据: 长度=%d字节，不是%d的倍数\n", len(peers), PeerLen)
		return nil
	}

	fmt.Printf("[DEBUG] 解析%d个peer信息\n", num)
	infos := make([]PeerInfo, 0, num) // 使用0初始容量，仅分配空间

	for i := 0; i < num; i++ {
		offset := i * PeerLen
		// 提取IP和端口
		ip := net.IP(peers[offset : offset+IpLen])
		port := binary.BigEndian.Uint16(peers[offset+IpLen : offset+PeerLen])

		// 验证IP和端口的有效性
		if ip.IsUnspecified() || ip.IsLoopback() || port == 0 {
			// 跳过无效的IP或端口
			continue
		}

		// 添加有效的peer信息
		infos = append(infos, PeerInfo{
			Ip:       ip,
			Port:     port,
			LastSeen: time.Now(),
		})
	}

	fmt.Printf("[DEBUG] 成功解析%d个有效peer (过滤掉%d个无效peer)\n", len(infos), num-len(infos))
	return infos
}

// 从tracker.txt文件中读取备用tracker服务器地址列表
func readBackupTrackers(filePath string) []string {
	file, err := os.Open(filePath)
	if err != nil {
		fmt.Println("无法打开tracker.txt文件: " + err.Error())
		return nil
	}
	defer file.Close()

	var trackers []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		tracker := strings.TrimSpace(scanner.Text())
		if tracker != "" {
			trackers = append(trackers, tracker)
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Println("读取tracker.txt文件出错: " + err.Error())
		return nil
	}

	return trackers
}

// 尝试连接tracker服务器并获取peer信息
// 返回peer信息列表和tracker URL
func tryTracker(announceURL string, tf *TorrentFile, peerId [IDLEN]byte) ([]PeerInfo, string) {
	// 临时替换Announce URL
	originalAnnounce := tf.Announce
	tf.Announce = announceURL
	defer func() { tf.Announce = originalAnnounce }()

	// 判断协议类型
	if strings.HasPrefix(announceURL, "udp://") {
		fmt.Printf("[DEBUG] 尝试连接UDP Tracker: %s\n", announceURL)
		peers, _ := tryUDPTracker(announceURL, tf, peerId)
		return peers, announceURL
	}

	// 检查URL协议支持
	if !strings.HasPrefix(announceURL, "http://") && !strings.HasPrefix(announceURL, "https://") {
		fmt.Printf("[ERROR] 不支持的Tracker协议: %s\n", announceURL)
		return nil, announceURL
	}

	// HTTP协议处理
	fmt.Printf("[DEBUG] 尝试连接HTTP Tracker: %s\n", announceURL)
	url, err := buildUrl(tf, peerId)
	if err != nil {
		fmt.Printf("[ERROR] 构建Tracker URL出错: %s, 错误: %s\n", announceURL, err.Error())
		return nil, announceURL
	}

	// 创建HTTP客户端，增加超时设置和重试机制
	cli := &http.Client{
		Timeout: 30 * time.Second, // 增加超时时间
		Transport: &http.Transport{
			ResponseHeaderTimeout: 20 * time.Second,
			DisableKeepAlives:     true, // 禁用连接复用，避免连接池问题
		},
	}

	// 发送HTTP请求
	fmt.Printf("[DEBUG] 发送HTTP请求: %s\n", url)
	resp, err := cli.Get(url)
	if err != nil {
		fmt.Printf("[ERROR] 无法连接到Tracker服务器: %s, 错误: %s\n", announceURL, err.Error())
		return nil, announceURL
	}
	defer resp.Body.Close()

	// 检查HTTP状态码
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("[ERROR] Tracker服务器返回非200状态码: %s, 状态码: %d\n", announceURL, resp.StatusCode)
		return nil, announceURL
	}

	// 解析响应
	trackResp := new(TrackerResp)
	err = bencode.Unmarshal(resp.Body, trackResp)
	if err != nil {
		fmt.Printf("[ERROR] Tracker响应解析错误: %s, 错误: %s\n", announceURL, err.Error())
		return nil, announceURL
	}

	// 检查是否有错误信息
	if trackResp.FailureReason != "" {
		fmt.Printf("[ERROR] Tracker返回错误: %s, 错误信息: %s\n", announceURL, trackResp.FailureReason)
		return nil, announceURL
	}

	// 检查是否有警告信息
	if trackResp.WarningMessage != "" {
		fmt.Printf("[WARNING] Tracker警告: %s, 警告信息: %s\n", announceURL, trackResp.WarningMessage)
	}

	// 检查peers数据是否为空
	if len(trackResp.Peers) == 0 {
		fmt.Printf("[WARNING] Tracker未返回任何Peer: %s\n", announceURL)
		return nil, announceURL
	}

	// 构建peer信息
	peers := buildPeerInfo([]byte(trackResp.Peers))
	if peers != nil {
		fmt.Printf("[INFO] 从Tracker获取到%d个Peer: %s\n", len(peers), announceURL)
		if trackResp.Complete > 0 || trackResp.Incomplete > 0 {
			fmt.Printf("[INFO] 做种数: %d, 下载数: %d\n", trackResp.Complete, trackResp.Incomplete)
		}
	}

	return peers, announceURL
}

// tryUDPTracker 尝试连接UDP协议的Tracker服务器
// 返回peer信息列表和tracker URL
func tryUDPTracker(announceURL string, tf *TorrentFile, peerId [IDLEN]byte) ([]PeerInfo, string) {
	// 解析UDP地址
	u, err := url.Parse(announceURL)
	if err != nil {
		fmt.Printf("[ERROR] 解析UDP地址错误: %s, 错误: %s\n", announceURL, err.Error())
		return nil, announceURL
	}

	// 提取主机和端口
	host := u.Host
	if !strings.Contains(host, ":") {
		// 如果没有指定端口，使用默认端口80
		host = host + ":80"
	}
	fmt.Printf("[DEBUG] 连接UDP Tracker: %s (主机: %s)\n", announceURL, host)

	// 设置最大重试次数
	maxRetries := 3
	var conn net.Conn

	// 尝试连接，带重试
	for i := 0; i < maxRetries; i++ {
		conn, err = net.DialTimeout("udp", host, 15*time.Second)
		if err == nil {
			break
		}
		fmt.Printf("[WARNING] 连接UDP Tracker失败(尝试 %d/%d): %s, 错误: %s\n", i+1, maxRetries, announceURL, err.Error())
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * time.Second) // 指数退避
		}
	}

	if err != nil {
		fmt.Printf("[ERROR] 连接UDP Tracker失败: %s, 已重试%d次\n", announceURL, maxRetries)
		return nil, announceURL
	}

	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))

	// 构建UDP请求 - 修正魔数格式
	transactionID := uint32(time.Now().UnixNano() & 0xFFFFFFFF) // 使用纳秒时间戳以避免冲突
	buf := make([]byte, 16)
	// 正确的魔数是 0x41727101980，十六进制表示为 0x0000041727101980
	binary.BigEndian.PutUint64(buf[0:8], 0x0000041727101980) // connection_id
	binary.BigEndian.PutUint32(buf[8:12], 0)                 // action=connect
	binary.BigEndian.PutUint32(buf[12:16], transactionID)

	fmt.Printf("[DEBUG] 发送UDP连接请求，事务ID: %d\n", transactionID)

	// 发送连接请求，带重试
	for i := 0; i < maxRetries; i++ {
		_, err = conn.Write(buf)
		if err == nil {
			break
		}
		fmt.Printf("[WARNING] 发送UDP连接请求失败(尝试 %d/%d): %s\n", i+1, maxRetries, err.Error())
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}

	if err != nil {
		fmt.Printf("[ERROR] 发送UDP连接请求失败: %s\n", err.Error())
		return nil, announceURL
	}

	// 读取连接响应，带重试
	resp := make([]byte, 16)
	var n int
	for i := 0; i < maxRetries; i++ {
		// 设置读取超时
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err = io.ReadFull(conn, resp)
		if err == nil && n == 16 {
			break
		}
		fmt.Printf("[WARNING] 读取UDP连接响应失败(尝试 %d/%d): %s\n", i+1, maxRetries, err.Error())
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * time.Second)
			// 重新发送连接请求
			_, err = conn.Write(buf)
			if err != nil {
				fmt.Printf("[ERROR] 重新发送UDP连接请求失败: %s\n", err.Error())
				continue
			}
		}
	}

	if err != nil || n != 16 {
		fmt.Printf("[ERROR] 读取UDP连接响应失败: %s\n", err.Error())
		return nil, announceURL
	}

	// 解析连接响应
	action := binary.BigEndian.Uint32(resp[0:4])
	respTransactionID := binary.BigEndian.Uint32(resp[4:8])

	if action != 0 || respTransactionID != transactionID {
		fmt.Printf("[ERROR] UDP连接响应无效: action=%d, 收到事务ID=%d, 期望事务ID=%d\n",
			action, respTransactionID, transactionID)
		return nil, announceURL
	}

	connectionID := binary.BigEndian.Uint64(resp[8:16])
	fmt.Printf("[DEBUG] 收到UDP连接响应，连接ID: %d\n", connectionID)

	// 构建announce请求
	announceBuf := make([]byte, 98)
	binary.BigEndian.PutUint64(announceBuf[0:8], connectionID)
	binary.BigEndian.PutUint32(announceBuf[8:12], 1) // action=announce
	// 使用新的事务ID
	transactionID = uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	binary.BigEndian.PutUint32(announceBuf[12:16], transactionID)

	// 复制info_hash和peer_id
	copy(announceBuf[16:36], tf.InfoSHA[:]) // info_hash
	copy(announceBuf[36:56], peerId[:])     // peer_id

	// 设置下载、剩余和上传字节数
	binary.BigEndian.PutUint64(announceBuf[56:64], 0)                  // downloaded
	binary.BigEndian.PutUint64(announceBuf[64:72], uint64(tf.FileLen)) // left - 使用实际文件大小
	binary.BigEndian.PutUint64(announceBuf[72:80], 0)                  // uploaded

	// 设置事件和其他参数
	binary.BigEndian.PutUint32(announceBuf[80:84], 0)                // event=0 (none)
	binary.BigEndian.PutUint32(announceBuf[84:88], 0)                // IP address=0 (默认)
	binary.BigEndian.PutUint32(announceBuf[88:92], 0)                // key (随机值)
	binary.BigEndian.PutUint32(announceBuf[92:96], 100)              // num_want - 请求更多peer
	binary.BigEndian.PutUint16(announceBuf[96:98], uint16(PeerPort)) // port

	fmt.Printf("[DEBUG] 发送UDP announce请求，事务ID: %d\n", transactionID)

	// 发送announce请求，带重试
	for i := 0; i < maxRetries; i++ {
		_, err = conn.Write(announceBuf)
		if err == nil {
			break
		}
		fmt.Printf("[WARNING] 发送UDP announce请求失败(尝试 %d/%d): %s\n", i+1, maxRetries, err.Error())
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}

	if err != nil {
		fmt.Printf("[ERROR] 发送UDP announce请求失败: %s\n", err.Error())
		return nil, announceURL
	}

	// 读取announce响应
	// 首先读取头部(20字节)以获取实际peer数量
	headerBuf := make([]byte, 20)
	for i := 0; i < maxRetries; i++ {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err = io.ReadFull(conn, headerBuf)
		if err == nil && n == 20 {
			break
		}
		fmt.Printf("[WARNING] 读取UDP announce响应头部失败(尝试 %d/%d): %s\n", i+1, maxRetries, err.Error())
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * time.Second)
			// 重新发送announce请求
			_, err = conn.Write(announceBuf)
			if err != nil {
				fmt.Printf("[ERROR] 重新发送UDP announce请求失败: %s\n", err.Error())
				continue
			}
		}
	}

	if err != nil || n != 20 {
		fmt.Printf("[ERROR] 读取UDP announce响应头部失败: %s\n", err.Error())
		return nil, announceURL
	}

	// 解析头部
	action = binary.BigEndian.Uint32(headerBuf[0:4])
	respTransactionID = binary.BigEndian.Uint32(headerBuf[4:8])
	interval := binary.BigEndian.Uint32(headerBuf[8:12])
	leechers := binary.BigEndian.Uint32(headerBuf[12:16])
	seeders := binary.BigEndian.Uint32(headerBuf[16:20])

	if action != 1 || respTransactionID != transactionID {
		fmt.Printf("[ERROR] UDP announce响应头部无效: action=%d, 收到事务ID=%d, 期望事务ID=%d\n",
			action, respTransactionID, transactionID)
		return nil, announceURL
	}

	fmt.Printf("[INFO] UDP Tracker响应: 间隔=%d秒, 做种数=%d, 下载数=%d\n",
		interval, seeders, leechers)

	// 读取peer列表
	// 计算预期的peer数量 (每个peer占6字节)
	expectedPeers := int(seeders + leechers)
	if expectedPeers > 1000 { // 限制最大peer数量
		expectedPeers = 1000
	}
	if expectedPeers == 0 {
		fmt.Printf("[WARNING] UDP Tracker未返回任何Peer: %s\n", announceURL)
		return nil, announceURL
	}

	// 分配足够大的缓冲区
	peersBuf := make([]byte, expectedPeers*6)

	// 尝试读取所有peer数据
	var peerBytes []byte
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = conn.Read(peersBuf)
	if err != nil && err != io.EOF {
		fmt.Printf("[ERROR] 读取UDP announce响应peer列表失败: %s\n", err.Error())
		// 尝试使用已读取的数据
		if n > 0 && n%6 == 0 {
			fmt.Printf("[WARNING] 部分读取成功，尝试使用已读取的%d字节数据\n", n)
			peerBytes = peersBuf[:n]
		} else {
			return nil, announceURL
		}
	} else {
		peerBytes = peersBuf[:n]
	}

	// 检查读取的数据是否为peer列表的倍数
	if len(peerBytes)%6 != 0 {
		fmt.Printf("[ERROR] 收到的peer数据长度不是6的倍数: %d字节\n", len(peerBytes))
		return nil, announceURL
	}

	// 解析peer列表
	peers := buildPeerInfo(peerBytes)
	if peers != nil && len(peers) > 0 {
		fmt.Printf("[INFO] 从UDP Tracker获取到%d个Peer: %s\n", len(peers), announceURL)
	} else {
		fmt.Printf("[WARNING] 从UDP Tracker获取的Peer列表为空: %s\n", announceURL)
	}

	return peers, announceURL
}

// FindPeers 从多个tracker服务器获取peer信息
// 会显示每个tracker服务器提供的peer数量和连接状态
func FindPeers(tf *TorrentFile, peerId [IDLEN]byte) []PeerInfo {
	fmt.Println("开始查找Peers...")

	// 获取所有Tracker服务器地址
	trackers := []string{tf.Announce}
	backupTrackers := readBackupTrackers(".\\tracker.txt")
	if backupTrackers != nil {
		fmt.Printf("[INFO] 从tracker.txt加载了%d个备用Tracker\n", len(backupTrackers))
		trackers = append(trackers, backupTrackers...)
	} else {
		fmt.Println("[WARNING] 未能加载备用Tracker列表，仅使用种子文件中的Tracker")
	}

	fmt.Printf("[INFO] 共有%d个Tracker服务器待连接\n", len(trackers))

	// 创建channel用于接收结果，设置足够大的缓冲区避免goroutine阻塞
	type trackerResult struct {
		peers      []PeerInfo
		trackerURL string
		success    bool
		errorMsg   string
	}
	resultChan := make(chan trackerResult, len(trackers))
	var allPeers []PeerInfo

	// 设置超时控制
	timeout := 60 * time.Second // 总体超时时间
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 并发请求所有Tracker服务器
	for _, tracker := range trackers {
		go func(url string) {
			// 使用独立的goroutine处理每个tracker请求
			start := time.Now()
			peers, trackerURL := tryTracker(url, tf, peerId)
			elapsed := time.Since(start)

			result := trackerResult{
				peers:      peers,
				trackerURL: trackerURL,
				success:    peers != nil && len(peers) > 0,
			}

			if !result.success {
				result.errorMsg = "未返回有效Peer"
			}

			// 记录请求耗时
			fmt.Printf("[DEBUG] Tracker请求耗时: %s - %v\n", url, elapsed)

			// 发送结果到channel
			select {
			case resultChan <- result:
				// 成功发送
			case <-ctx.Done():
				// 超时或取消
				fmt.Printf("[WARNING] Tracker请求被取消: %s\n", url)
				return
			}
		}(tracker)
	}

	// 收集并合并结果
	peerSet := make(map[string]PeerInfo) // 使用map去重
	trackerStats := make(map[string]struct {
		count   int
		success bool
		error   string
	}) // 记录每个tracker的状态和提供的peer数量

	// 设置等待超时
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	// 计数器，用于跟踪已处理的tracker数量
	processedTrackers := 0
	successTrackers := 0

	// 收集结果，直到所有tracker都返回或超时
	for processedTrackers < len(trackers) {
		select {
		case result := <-resultChan:
			processedTrackers++

			// 记录tracker状态
			stat := trackerStats[result.trackerURL]
			stat.success = result.success
			stat.error = result.errorMsg

			// 处理peer结果
			if result.peers != nil && len(result.peers) > 0 {
				successTrackers++
				peerCount := 0
				for _, peer := range result.peers {
					key := peer.Ip.String() + ":" + strconv.Itoa(int(peer.Port))
					if _, exists := peerSet[key]; !exists {
						peerSet[key] = peer
						allPeers = append(allPeers, peer)
						peerCount++
					}
				}
				stat.count = peerCount
				trackerStats[result.trackerURL] = stat

				// 如果已经获取了足够多的peer，可以提前结束
				if len(allPeers) >= 200 {
					fmt.Printf("[INFO] 已获取足够的Peer(%d个)，停止等待其他Tracker\n", len(allPeers))
					break
				}
			} else {
				trackerStats[result.trackerURL] = stat
			}

			// 显示进度
			fmt.Printf("[INFO] Tracker进度: %d/%d (成功: %d)\n", processedTrackers, len(trackers), successTrackers)

		case <-timeoutTimer.C:
			// 超时处理
			fmt.Printf("[WARNING] 等待Tracker响应超时，已处理%d/%d个Tracker\n", processedTrackers, len(trackers))
			goto CollectDone
		}
	}

CollectDone:
	// 打印每个tracker的贡献和状态
	if len(trackerStats) > 0 {
		fmt.Println("\n各Tracker服务器状态:")

		// 按照peer数量排序
		type trackerStatPair struct {
			url  string
			stat struct {
				count   int
				success bool
				error   string
			}
		}

		statPairs := make([]trackerStatPair, 0, len(trackerStats))
		for url, stat := range trackerStats {
			statPairs = append(statPairs, trackerStatPair{url, stat})
		}

		// 按peer数量降序排序
		sort.Slice(statPairs, func(i, j int) bool {
			return statPairs[i].stat.count > statPairs[j].stat.count
		})

		// 打印结果
		for _, pair := range statPairs {
			if pair.stat.success {
				fmt.Printf("  [成功] %s: %d个Peer\n", pair.url, pair.stat.count)
			} else {
				fmt.Printf("  [失败] %s: %s\n", pair.url, pair.stat.error)
			}
		}

		// 打印总结
		fmt.Printf("\n[总结] 成功连接%d/%d个Tracker，获取到%d个唯一Peer\n",
			successTrackers, len(trackers), len(allPeers))

		if len(allPeers) > 0 {
			return allPeers
		}
	}

	fmt.Println("[ERROR] 所有Tracker服务器均未返回有效Peer")
	return nil
}
