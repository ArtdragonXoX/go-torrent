package torrent

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackpal/bencode-go"
)

const (
	PeerPort int = 6666
	IpLen    int = 4
	PortLen  int = 2
	PeerLen  int = IpLen + PortLen
)

const IDLEN int = 20

type TrackerResp struct {
	Interval int    `bencode:"interval"`
	Peers    string `bencode:"peers"`
}

// TrackerResponse 表示 Tracker 原始响应数据
type TrackerResponse struct {
	Data []byte // 原始响应内容
	From string // 标记来源协议 (http/udp)
}

// AnnounceToTracker 根据 Tracker URL 自动选择协议进行请求
func AnnounceToTracker(trackerURL string, infoHash []byte, peerID string, port int) (*TrackerResponse, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid tracker URL: %w", err)
	}

	switch u.Scheme {
	case "http", "https":
		return announceHTTP(trackerURL, infoHash, peerID, port)
	case "udp":
		return announceUDP(trackerURL, infoHash, peerID, port)
	default:
		return nil, errors.New("unsupported tracker protocol")
	}
}

// ================= HTTP/HTTPS Tracker 交互 =================
func announceHTTP(trackerURL string, infoHash []byte, peerID string, port int) (*TrackerResponse, error) {
	// 构建请求参数
	params := url.Values{
		"info_hash":  []string{string(infoHash)}, // 注意这里需要原生字节，不要编码
		"peer_id":    []string{peerID},
		"port":       []string{strconv.Itoa(port)},
		"uploaded":   []string{"0"},
		"downloaded": []string{"0"},
		"left":       []string{"0"},
		"compact":    []string{"1"},
		"event":      []string{"started"},
	}

	// 构造完整 URL（需要手动处理 info_hash 的特殊编码）
	baseURL, err := url.Parse(trackerURL)
	if err != nil {
		return nil, err
	}
	baseURL.RawQuery = params.Encode()

	// 特殊处理 info_hash 的编码（替换为字节的百分号编码）
	rawQuery := baseURL.Query().Encode()
	rawQuery = fixInfoHashEncoding(rawQuery, infoHash)
	baseURL.RawQuery = rawQuery

	// 创建 HTTP 客户端
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 跳过 HTTPS 证书验证
		},
		Timeout: 30 * time.Second,
	}

	// 发送请求
	req, _ := http.NewRequest("GET", baseURL.String(), nil)
	req.Header.Set("User-Agent", "GoTorrent/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tracker returned status: %s", resp.Status)
	}

	// 读取响应内容
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return &TrackerResponse{Data: data, From: "http"}, nil
}

// 修复 info_hash 的 URL 编码（Go 的 url.Values 会错误编码二进制数据）
func fixInfoHashEncoding(rawQuery string, infoHash []byte) string {
	// 查找并替换 info_hash 参数的正确编码
	prefix := "info_hash="
	start := bytes.Index([]byte(rawQuery), []byte(prefix))
	if start == -1 {
		return rawQuery
	}

	start += len(prefix)
	end := start + bytes.IndexByte([]byte(rawQuery[start:]), '&')
	if end < start {
		end = len(rawQuery)
	}

	// 替换为正确的百分号编码
	encodedHash := url.QueryEscape(string(infoHash))
	return rawQuery[:start] + encodedHash + rawQuery[end:]
}

// ================= UDP Tracker 交互 =================
func announceUDP(trackerURL string, infoHash []byte, peerID string, port int) (*TrackerResponse, error) {
	// 使用url.Parse正确解析UDP地址
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid UDP tracker URL: %w", err)
	}

	// 提取主机名和端口
	hostport := u.Host
	addr, err := net.ResolveUDPAddr("udp", hostport)
	if err != nil {
		return nil, fmt.Errorf("invalid UDP address: %w", err)
	}

	// 创建 UDP 连接
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("UDP connection failed: %w", err)
	}
	defer conn.Close()

	// 第一步：发送连接请求
	connectionID, err := udpConnect(conn)
	if err != nil {
		return nil, fmt.Errorf("UDP connect failed: %w", err)
	}

	// 第二步：发送公告请求
	respData, err := udpAnnounce(conn, connectionID, infoHash, peerID, port)
	if err != nil {
		return nil, fmt.Errorf("UDP announce failed: %w", err)
	}

	return &TrackerResponse{Data: respData, From: "udp"}, nil
}

// UDP 连接阶段（获取 connection_id）
func udpConnect(conn *net.UDPConn) (uint64, error) {
	// 构建连接请求
	req := make([]byte, 16)
	binary.BigEndian.PutUint64(req[0:8], 0x41727101980) // 协议魔数
	binary.BigEndian.PutUint32(req[8:12], 0)            // 动作 (0=connect)
	transactionID := uint32(time.Now().Unix())
	binary.BigEndian.PutUint32(req[12:16], transactionID)

	// 发送请求
	if _, err := conn.Write(req); err != nil {
		return 0, err
	}

	// 设置读取超时（按 BitTorrent 规范实现指数退避）
	timeout := 15 * time.Second
	retries := 0
	for {
		conn.SetReadDeadline(time.Now().Add(timeout))
		resp := make([]byte, 16)
		n, err := conn.Read(resp)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if retries >= 8 {
					return 0, errors.New("UDP connect timeout")
				}
				timeout *= 2
				retries++
				continue
			}
			return 0, err
		}

		if n != 16 {
			return 0, errors.New("invalid connect response length")
		}

		// 验证事务ID
		respAction := binary.BigEndian.Uint32(resp[0:4])
		respTransID := binary.BigEndian.Uint32(resp[4:8])
		if respAction != 0 || respTransID != transactionID {
			return 0, errors.New("invalid connect response")
		}

		return binary.BigEndian.Uint64(resp[8:16]), nil // 返回 connection_id
	}
}

// UDP 公告阶段
func udpAnnounce(conn *net.UDPConn, connectionID uint64, infoHash []byte, peerID string, port int) ([]byte, error) {
	// 构建公告请求
	req := make([]byte, 98)
	binary.BigEndian.PutUint64(req[0:8], connectionID)
	binary.BigEndian.PutUint32(req[8:12], 1) // 动作 (1=announce)
	transactionID := uint32(time.Now().Unix())
	binary.BigEndian.PutUint32(req[12:16], transactionID)

	copy(req[16:36], infoHash)                           // info_hash
	copy(req[36:56], peerID)                             // peer_id
	binary.BigEndian.PutUint64(req[56:64], 0)            // downloaded
	binary.BigEndian.PutUint64(req[64:72], 0)            // left
	binary.BigEndian.PutUint64(req[72:80], 0)            // uploaded
	binary.BigEndian.PutUint32(req[80:84], 0)            // event (0=none)
	binary.BigEndian.PutUint32(req[84:88], 0)            // IP address (0=default)
	binary.BigEndian.PutUint32(req[88:92], 0)            // key
	binary.BigEndian.PutUint32(req[92:96], ^uint32(0))   // num_want (-1)
	binary.BigEndian.PutUint16(req[96:98], uint16(port)) // port

	// 发送请求
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}

	// 读取响应（带重试机制）
	timeout := 15 * time.Second
	retries := 0
	for {
		conn.SetReadDeadline(time.Now().Add(timeout))
		resp := make([]byte, 4096)
		n, err := conn.Read(resp)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if retries >= 8 {
					return nil, errors.New("UDP announce timeout")
				}
				timeout *= 2
				retries++
				continue
			}
			return nil, err
		}

		if n < 20 {
			return nil, errors.New("invalid announce response length")
		}

		// 验证事务ID
		respAction := binary.BigEndian.Uint32(resp[0:4])
		respTransID := binary.BigEndian.Uint32(resp[4:8])
		if respAction != 1 || respTransID != transactionID {
			return nil, errors.New("invalid announce response")
		}

		return resp[20:n], nil // 返回 Peer 数据部分
	}
}

// ParsePeersFromResponse 根据 Tracker 响应解析 Peer 列表
func ParsePeersFromResponse(resp *TrackerResponse) ([]PeerInfo, error) {
	switch resp.From {
	case "http":
		return parseHTTPPeers(resp.Data)
	case "udp":
		return parseUDPPeers(resp.Data)
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", resp.From)
	}
}

// ================= HTTP/HTTPS 响应解析 =================
func parseHTTPPeers(data []byte) ([]PeerInfo, error) {
	// Bencode 解码
	decoded, err := bencode.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("bencode decode failed: %w", err)
	}

	responseDict, ok := decoded.(map[string]interface{})
	if !ok {
		return nil, errors.New("invalid tracker response format")
	}

	var peers []PeerInfo

	// 解析二进制格式的 peers（IPv4）
	if peersBinary, ok := responseDict["peers"].(string); ok {
		parsed, err := parseBinaryPeers([]byte(peersBinary), 6) // IPv4: 6 bytes/peer
		if err != nil {
			return nil, fmt.Errorf("parse IPv4 peers failed: %w", err)
		}
		peers = append(peers, parsed...)
	}

	// 解析二进制格式的 peers6（IPv6）
	if peers6Binary, ok := responseDict["peers6"].(string); ok {
		parsed, err := parseBinaryPeers([]byte(peers6Binary), 18) // IPv6: 18 bytes/peer
		if err != nil {
			return nil, fmt.Errorf("parse IPv6 peers failed: %w", err)
		}
		peers = append(peers, parsed...)
	}

	// 解析字典列表格式的 peers
	if peersList, ok := responseDict["peers"].([]interface{}); ok {
		parsed, err := parseDictPeers(peersList)
		if err != nil {
			return nil, fmt.Errorf("parse dict peers failed: %w", err)
		}
		peers = append(peers, parsed...)
	}

	if len(peers) == 0 {
		return nil, errors.New("no peers found in response")
	}
	return peers, nil
}

// ================= UDP 响应解析 =================
func parseUDPPeers(data []byte) ([]PeerInfo, error) {
	// UDP 响应结构（BEP 15）：
	// Offset  Size    Name
	// 0       32-bit  action (1 = announce)
	// 4       32-bit  transaction_id
	// 8       32-bit  interval
	// 12      32-bit  leechers
	// 16      32-bit  seeders
	// 20 + 6 * n      IPv4 peers
	// 20 + 18 * n     IPv6 peers

	if len(data) < 20 {
		return nil, errors.New("invalid UDP response length")
	}

	// 提取 Peer 数据部分
	peersData := data[20:]

	// 自动检测 Peer 类型
	var peerSize int
	switch {
	case len(peersData)%6 == 0: // IPv4
		peerSize = 6
	case len(peersData)%18 == 0: // IPv6
		peerSize = 18
	default:
		return nil, errors.New("invalid UDP peers data length")
	}

	return parseBinaryPeers(peersData, peerSize)
}

// ================= 通用解析工具 =================
// parseBinaryPeers 解析二进制格式的 Peer 数据
func parseBinaryPeers(data []byte, peerSize int) ([]PeerInfo, error) {
	if len(data)%peerSize != 0 {
		return nil, fmt.Errorf("invalid binary data length: %d for peer size %d", len(data), peerSize)
	}

	peers := make([]PeerInfo, 0, len(data)/peerSize)
	for i := 0; i < len(data); i += peerSize {
		var ip net.IP
		var portBytes []byte

		switch peerSize {
		case 6: // IPv4
			ip = net.IPv4(data[i], data[i+1], data[i+2], data[i+3])
			portBytes = data[i+4 : i+6]
		case 18: // IPv6
			ip = make(net.IP, 16)
			copy(ip, data[i:i+16])
			portBytes = data[i+16 : i+18]
		default:
			return nil, errors.New("unsupported peer size")
		}

		port := int(binary.BigEndian.Uint16(portBytes))
		peers = append(peers, PeerInfo{Ip: ip, Port: uint16(port)})
	}
	return peers, nil
}

// parseDictPeers 解析字典列表格式的 Peer 数据
func parseDictPeers(peersList []interface{}) ([]PeerInfo, error) {
	peers := make([]PeerInfo, 0, len(peersList))
	for _, p := range peersList {
		peerDict, ok := p.(map[string]interface{})
		if !ok {
			return nil, errors.New("invalid peer dict entry")
		}

		// 提取 IP 地址
		ipStr, ok := peerDict["ip"].(string)
		if !ok {
			return nil, errors.New("peer dict missing ip")
		}
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address: %s", ipStr)
		}

		// 提取端口
		port, ok := peerDict["port"].(int64)
		if !ok {
			return nil, errors.New("peer dict missing port")
		}

		peers = append(peers, PeerInfo{Ip: ip, Port: uint16(port)})
	}
	return peers, nil
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

/**
 * TrackerResult 表示单个Tracker的请求结果
 */
type TrackerResult struct {
	URL   string     // Tracker地址
	Peers []PeerInfo // 获取到的Peer列表
}
