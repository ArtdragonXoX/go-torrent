package torrent

import (
	"bufio"
	"bytes"
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
	PeerPort  int = 6666
	IpV4Len   int = 4
	IpV6Len   int = 16
	PortLen   int = 2
	PeerV4Len int = IpV4Len + PortLen
	PeerV6Len int = IpV6Len + PortLen
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
	Peers6         string `bencode:"peers6"` // IPv6 peers列表
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
	// 添加ipv6=1参数，表示客户端支持IPv6
	params.Set("ipv6", "1")

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
// 支持三种格式：
// 1. IPv4格式：每个peer占用6字节，前4字节为IPv4地址，后2字节为端口
// 2. IPv6格式：每个peer占用18字节，前16字节为IPv6地址，后2字节为端口
// 3. 混合格式：同时包含IPv4和IPv6格式的peer数据
// 返回解析后的PeerInfo结构体切片
func buildPeerInfo(peers []byte) []PeerInfo {
	// 检查输入数据
	if len(peers) == 0 {
		fmt.Printf("[ERROR] 收到空的peers数据\n")
		return nil
	}

	// 尝试判断数据格式
	var infos []PeerInfo

	// 首先检查是否为BEP-32格式的IPv6数据（以0x02开头）
	if len(peers) > 0 && peers[0] == 0x02 && (len(peers)-1)%PeerV6Len == 0 {
		// 跳过第一个字节，处理纯IPv6数据
		fmt.Printf("[DEBUG] 检测到BEP-32格式的IPv6 peers数据\n")
		infos = parseIPv6Peers(peers[1:])
		return infos
	}

	// 检查是否为纯IPv4或纯IPv6格式
	if len(peers)%PeerV4Len == 0 {
		// 可能是纯IPv4格式
		fmt.Printf("[DEBUG] 检测到可能是IPv4格式的peers数据\n")
		infos = parseIPv4Peers(peers)
	} else if len(peers)%PeerV6Len == 0 {
		// 可能是纯IPv6格式
		fmt.Printf("[DEBUG] 检测到可能是IPv6格式的peers数据\n")
		infos = parseIPv6Peers(peers)
	} else {
		// 尝试解析为混合格式
		fmt.Printf("[DEBUG] 尝试解析混合格式的peers数据 (长度=%d字节)\n", len(peers))

		// 先尝试提取IPv4部分（假设在前面）
		var ipv4Data, ipv6Data []byte

		// 计算可能的IPv4数据长度（必须是PeerV4Len的倍数）
		ipv4Len := (len(peers) / PeerV4Len) * PeerV4Len

		// 检查剩余数据是否符合IPv6格式
		if (len(peers)-ipv4Len)%PeerV6Len == 0 && ipv4Len > 0 {
			// 可能是先IPv4后IPv6的混合格式
			ipv4Data = peers[:ipv4Len]
			ipv6Data = peers[ipv4Len:]

			fmt.Printf("[DEBUG] 检测到混合格式: IPv4数据(%d字节) + IPv6数据(%d字节)\n",
				len(ipv4Data), len(ipv6Data))

			// 解析IPv4部分
			if len(ipv4Data) > 0 {
				ipv4Peers := parseIPv4Peers(ipv4Data)
				infos = append(infos, ipv4Peers...)
			}

			// 解析IPv6部分
			if len(ipv6Data) > 0 {
				ipv6Peers := parseIPv6Peers(ipv6Data)
				infos = append(infos, ipv6Peers...)
			}
		} else {
			// 尝试其他可能的分割点
			found := false

			// 从后向前尝试不同的分割点
			for i := len(peers); i >= PeerV6Len; i -= PeerV6Len {
				if i%PeerV4Len == 0 {
					// 找到一个可能的分割点
					ipv4Data = peers[:i]
					ipv6Data = peers[i:]

					if len(ipv4Data)%PeerV4Len == 0 && len(ipv6Data)%PeerV6Len == 0 {
						fmt.Printf("[DEBUG] 尝试分割点: IPv4数据(%d字节) + IPv6数据(%d字节)\n",
							len(ipv4Data), len(ipv6Data))

						// 解析IPv4部分
						if len(ipv4Data) > 0 {
							ipv4Peers := parseIPv4Peers(ipv4Data)
							infos = append(infos, ipv4Peers...)
						}

						// 解析IPv6部分
						if len(ipv6Data) > 0 {
							ipv6Peers := parseIPv6Peers(ipv6Data)
							infos = append(infos, ipv6Peers...)
						}

						found = true
						break
					}
				}
			}

			if !found {
				fmt.Printf("[ERROR] 无法解析混合格式的peers数据: 长度=%d字节，既不是IPv4(%d的倍数)也不是IPv6(%d的倍数)\n",
					len(peers), PeerV4Len, PeerV6Len)
				return nil
			}
		}
	}

	// 输出解析结果统计
	if len(infos) > 0 {
		// 统计IPv4和IPv6地址数量
		var ipv4Count, ipv6Count int
		for _, p := range infos {
			if p.Ip.To4() != nil {
				ipv4Count++
			} else if p.Ip.To16() != nil {
				ipv6Count++
			}
		}
		fmt.Printf("[DEBUG] 成功解析%d个有效peer (IPv4: %d, IPv6: %d)\n",
			len(infos), ipv4Count, ipv6Count)
	} else {
		fmt.Printf("[WARNING] 未能解析出任何有效peer\n")
	}

	return infos
}

// parseIPv4Peers 解析IPv4格式的peer数据
// 每个peer占用6字节：4字节IPv4地址 + 2字节端口
func parseIPv4Peers(data []byte) []PeerInfo {
	if len(data) == 0 || len(data)%PeerV4Len != 0 {
		return nil
	}

	num := len(data) / PeerV4Len
	fmt.Printf("[DEBUG] 解析%d个IPv4 peer信息\n", num)
	infos := make([]PeerInfo, 0, num)

	for i := 0; i < num; i++ {
		offset := i * PeerV4Len

		// 提取IPv4地址和端口
		ip := net.IP(data[offset : offset+IpV4Len])
		port := binary.BigEndian.Uint16(data[offset+IpV4Len : offset+PeerV4Len])

		// 验证IP和端口的有效性
		if ip == nil || port == 0 {
			continue
		}

		// 确保是IPv4地址
		ipv4 := ip.To4()
		if ipv4 == nil {
			continue
		}

		// 添加有效的peer信息
		infos = append(infos, PeerInfo{
			Ip:       ipv4,
			Port:     port,
			LastSeen: time.Now(),
		})
	}

	return infos
}

// parseIPv6Peers 解析IPv6格式的peer数据
// 每个peer占用18字节：16字节IPv6地址 + 2字节端口
func parseIPv6Peers(data []byte) []PeerInfo {
	if len(data) == 0 || len(data)%PeerV6Len != 0 {
		return nil
	}

	num := len(data) / PeerV6Len
	fmt.Printf("[DEBUG] 解析%d个IPv6 peer信息\n", num)
	infos := make([]PeerInfo, 0, num)

	for i := 0; i < num; i++ {
		offset := i * PeerV6Len

		// 提取IPv6地址和端口
		ip := net.IP(data[offset : offset+IpV6Len])
		port := binary.BigEndian.Uint16(data[offset+IpV6Len : offset+PeerV6Len])

		// 验证IP和端口的有效性
		if ip == nil || port == 0 {
			continue
		}

		// 确保是IPv6地址
		ipv6 := ip.To16()
		if ipv6 == nil || ip.To4() != nil { // 排除IPv4映射的IPv6地址
			continue
		}

		// 添加有效的peer信息
		infos = append(infos, PeerInfo{
			Ip:       ipv6,
			Port:     port,
			LastSeen: time.Now(),
		})
	}

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
	// 创建一个空的TorrentTask用于日志记录
	// 在FindPeers函数中会检查是否需要记录日志
	task := &TorrentTask{
		FileName:     tf.FileName,
		peerFailures: make(map[string]int),
	}

	// 检查命令行参数中是否有-log选项
	for _, arg := range os.Args {
		if arg == "-log" {
			task.EnableLog = true
			break
		}
	}
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

	// 读取响应体内容用于日志记录
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("[ERROR] 读取Tracker响应内容失败: %s, 错误: %s\n", announceURL, err.Error())
		return nil, announceURL
	}

	// 如果启用了日志记录，记录原始响应数据
	if task.EnableLog {
		peerKey := fmt.Sprintf("Tracker:%s", announceURL)
		// 记录tracker响应日志
		logTrackerResponse(task, peerKey, "HTTP_RESPONSE", string(respBody))
	}

	// 解析响应
	trackResp := new(TrackerResp)
	err = bencode.Unmarshal(bytes.NewReader(respBody), trackResp)
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
	hasIPv4Peers := len(trackResp.Peers) > 0
	hasIPv6Peers := len(trackResp.Peers6) > 0

	if !hasIPv4Peers && !hasIPv6Peers {
		fmt.Printf("[WARNING] Tracker未返回任何Peer: %s\n", announceURL)
		return nil, announceURL
	}

	// 构建peer信息
	var allPeers []PeerInfo

	// 处理IPv4 peers
	if hasIPv4Peers {
		peersV4 := buildPeerInfo([]byte(trackResp.Peers))
		if peersV4 != nil && len(peersV4) > 0 {
			fmt.Printf("[INFO] 从Tracker获取到%d个IPv4 Peer: %s\n", len(peersV4), announceURL)
			allPeers = append(allPeers, peersV4...)
		}
	}

	// 处理IPv6 peers
	if hasIPv6Peers {
		peersV6 := buildPeerInfo([]byte(trackResp.Peers6))
		if peersV6 != nil && len(peersV6) > 0 {
			fmt.Printf("[INFO] 从Tracker获取到%d个IPv6 Peer: %s\n", len(peersV6), announceURL)
			allPeers = append(allPeers, peersV6...)
		}
	}

	// 输出统计信息
	if len(allPeers) == 0 {
		fmt.Printf("[ERROR] 从Tracker未获取到任何有效Peer: %s\n", announceURL)
		return nil, announceURL
	}

	fmt.Printf("[INFO] 从Tracker总共获取到%d个Peer: %s\n", len(allPeers), announceURL)
	if trackResp.Complete > 0 || trackResp.Incomplete > 0 {
		fmt.Printf("[INFO] 做种数: %d, 下载数: %d\n", trackResp.Complete, trackResp.Incomplete)
	}
	return allPeers, announceURL
}

// tryUDPTracker 尝试连接UDP协议的Tracker服务器
// 返回peer信息列表和tracker URL
func tryUDPTracker(announceURL string, tf *TorrentFile, peerId [IDLEN]byte) ([]PeerInfo, string) {
	// 创建一个空的TorrentTask用于日志记录
	task := &TorrentTask{
		FileName:     tf.FileName,
		peerFailures: make(map[string]int),
	}

	// 检查命令行参数中是否有-log选项
	for _, arg := range os.Args {
		if arg == "-log" {
			task.EnableLog = true
			break
		}
	}
	// 如果启用了日志记录，记录UDP请求数据
	if task.EnableLog {
		peerKey := fmt.Sprintf("UDP_Tracker:%s", announceURL)
		logTrackerResponse(task, peerKey, "UDP_REQUEST", "开始连接UDP Tracker")
	}
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
			// 如果启用了日志记录，记录UDP响应头部数据
			if task.EnableLog {
				peerKey := fmt.Sprintf("UDP_Tracker:%s", announceURL)
				hexData := fmt.Sprintf("%X", headerBuf)
				logTrackerResponse(task, peerKey, "UDP_HEADER", hexData)
			}
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
	// 计算预期的peer数量
	// 注意：UDP tracker协议中，IPv6 peers通常会在响应中包含一个标识字节
	expectedPeers := int(seeders + leechers)
	if expectedPeers > 1000 { // 限制最大peer数量
		expectedPeers = 1000
	}
	if expectedPeers == 0 {
		fmt.Printf("[WARNING] UDP Tracker未返回任何Peer: %s\n", announceURL)
		return nil, announceURL
	}

	// 分配足够大的缓冲区 - 使用最大可能的大小 (IPv6)
	// 每个IPv6 peer占用18字节，为了安全起见，分配更大的缓冲区
	peersBuf := make([]byte, expectedPeers*PeerV6Len*2) // 分配足够大的缓冲区

	// 尝试读取所有peer数据
	var peerBytes []byte
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = conn.Read(peersBuf)
	if err != nil && err != io.EOF {
		fmt.Printf("[ERROR] 读取UDP announce响应peer列表失败: %s\n", err.Error())
		// 尝试使用已读取的数据
		if n > 0 {
			// 检查是否可能是有效的peer数据
			if n%PeerV4Len == 0 || n%PeerV6Len == 0 || (n > 1 && (n-1)%PeerV6Len == 0) {
				fmt.Printf("[WARNING] 部分读取成功，尝试使用已读取的%d字节数据\n", n)
				peerBytes = peersBuf[:n]
			} else {
				return nil, announceURL
			}
		} else {
			return nil, announceURL
		}
	} else {
		peerBytes = peersBuf[:n]
	}

	// 检查是否为BEP-32格式的IPv6响应 (第一个字节为0x02)
	if len(peerBytes) > 0 && peerBytes[0] == 0x02 {
		fmt.Printf("[DEBUG] 检测到BEP-32格式的IPv6 peers响应\n")
		// 跳过第一个字节
		peerBytes = peerBytes[1:]
	}

	// 检查读取的数据是否为peer列表的有效格式
	if len(peerBytes)%PeerV4Len != 0 && len(peerBytes)%PeerV6Len != 0 {
		fmt.Printf("[ERROR] 收到的peer数据长度既不是%d的倍数也不是%d的倍数: %d字节\n",
			PeerV4Len, PeerV6Len, len(peerBytes))
		return nil, announceURL
	}

	// 解析peer列表
	peers := buildPeerInfo(peerBytes)
	if peers != nil && len(peers) > 0 {
		// 检查是否包含IPv4和IPv6地址
		var ipv4Count, ipv6Count int
		for _, p := range peers {
			if p.Ip.To4() != nil {
				ipv4Count++
			} else if p.Ip.To16() != nil {
				ipv6Count++
			}
		}

		fmt.Printf("[INFO] 从UDP Tracker获取到%d个Peer: %s (IPv4: %d, IPv6: %d)\n",
			len(peers), announceURL, ipv4Count, ipv6Count)
		fmt.Printf("[DEBUG] 原始peer数据长度: %d字节, 解析出%d个peer\n", len(peerBytes), len(peers))
	} else {
		fmt.Printf("[WARNING] 从UDP Tracker获取的Peer列表为空: %s\n", announceURL)
	}

	return peers, announceURL
}

// 记录tracker响应日志
// logTrackerResponse 记录Tracker响应日志
// 参数:
//
//	t: TorrentTask实例
//	peerKey: 对等节点标识
//	msgType: 消息类型(HTTP_RESPONSE/UDP_REQUEST等)
//	data: 要记录的原始数据或解析后的TrackerResp结构体
func logTrackerResponse(t *TorrentTask, peerKey string, msgType string, data string) {
	if t == nil || !t.EnableLog {
		return
	}

	t.logMu.Lock()
	defer t.logMu.Unlock()

	if t.logFile == nil {
		// 创建日志文件
		logPath := fmt.Sprintf("%s_tracker.log", t.FileName)
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Printf("无法创建日志文件: %v\n", err)
			return
		}
		t.logFile = f
		fmt.Printf("[INFO] 已创建Tracker响应日志文件: %s\n", logPath)
	}

	// 如果是Tracker响应数据，尝试解析为TrackerResp结构体
	var logData string
	if msgType == "HTTP_RESPONSE" || msgType == "UDP_RESPONSE" {
		trackResp := new(TrackerResp)
		err := bencode.Unmarshal(bytes.NewReader([]byte(data)), trackResp)
		if err == nil {
			logData = fmt.Sprintf("解析结果: Interval=%d, Complete=%d, Incomplete=%d, Peers=%d, Peers6=%d",
				trackResp.Interval, trackResp.Complete, trackResp.Incomplete,
				len(trackResp.Peers)/6, len(trackResp.Peers6)/18)
			// 添加PeerInfo详细信息
			if len(trackResp.Peers) > 0 {
				peers := buildPeerInfo([]byte(trackResp.Peers))
				for i, peer := range peers {
					logData += fmt.Sprintf("\nPeer %d: IP=%s, Port=%d", i+1, peer.Ip, peer.Port)
				}
			}
			if len(trackResp.Peers6) > 0 {
				peers6 := buildPeerInfo([]byte(trackResp.Peers6))
				for i, peer := range peers6 {
					logData += fmt.Sprintf("\nPeer6 %d: IP=%s, Port=%d", i+1, peer.Ip, peer.Port)
				}
			}
			if trackResp.FailureReason != "" {
				logData += fmt.Sprintf(", FailureReason=%s", trackResp.FailureReason)
			}
			if trackResp.WarningMessage != "" {
				logData += fmt.Sprintf(", WarningMessage=%s", trackResp.WarningMessage)
			}
		} else {
			logData = data
		}
	} else {
		logData = data
	}

	logEntry := fmt.Sprintf("[%s] %s %s: %s\n",
		time.Now().Format(time.RFC3339), peerKey, msgType, logData)
	if _, err := t.logFile.WriteString(logEntry); err != nil {
		fmt.Printf("写入日志失败: %v\n", err)
	}
}

// FindPeers 从多个tracker服务器获取peer信息
// 会显示每个tracker服务器提供的peer数量和连接状态
// 支持IPv4和IPv6两种类型的peer
func FindPeers(tf *TorrentFile, peerId [IDLEN]byte) []PeerInfo {
	// 创建一个空的TorrentTask用于日志记录
	// 在main.go中会设置EnableLog字段
	task := &TorrentTask{
		FileName: tf.FileName,
	}

	// 检查命令行参数中是否有-log选项
	for _, arg := range os.Args {
		if arg == "-log" {
			task.EnableLog = true
			break
		}
	}
	fmt.Println("开始查找Peers (支持IPv4和IPv6)...")

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
			// 将task.EnableLog传递给tryTracker函数
			// 修改tryTracker函数签名，接收enableLog参数
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
					fmt.Println("[DEBUG] 从Tracker获取到Peer: ", peer.Ip.String(), ":", peer.Port)
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

		// 统计IPv4和IPv6地址数量
		var ipv4Count, ipv6Count int
		for _, peer := range allPeers {
			if peer.Ip.To4() != nil {
				ipv4Count++
			} else if peer.Ip.To16() != nil {
				ipv6Count++
			}
		}

		// 打印总结
		fmt.Printf("\n[总结] 成功连接%d/%d个Tracker，获取到%d个唯一Peer (IPv4: %d, IPv6: %d)\n",
			successTrackers, len(trackers), len(allPeers), ipv4Count, ipv6Count)

		if len(allPeers) > 0 {
			return allPeers
		}
	}

	fmt.Println("[ERROR] 所有Tracker服务器均未返回有效Peer")
	return nil
}
