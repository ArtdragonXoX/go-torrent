package torrent

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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
	Ip   net.IP
	Port uint16
}

type TrackerResp struct {
	Interval int    `bencode:"interval"`
	Peers    string `bencode:"peers"`
}

func buildUrl(tf *TorrentFile, peerId [IDLEN]byte) (string, error) {
	base, err := url.Parse(tf.Announce)
	if err != nil {
		fmt.Println("Announce Error: " + tf.Announce)
		return "", err
	}

	params := url.Values{
		"info_hash":  []string{string(tf.InfoSHA[:])},
		"peer_id":    []string{string(peerId[:])},
		"port":       []string{strconv.Itoa(PeerPort)},
		"uploaded":   []string{"0"},
		"downloaded": []string{"0"},
		"compact":    []string{"1"},
		"left":       []string{strconv.Itoa(tf.FileLen)},
	}

	base.RawQuery = params.Encode()
	return base.String(), nil
}

func buildPeerInfo(peers []byte) []PeerInfo {
	num := len(peers) / PeerLen
	if len(peers)%PeerLen != 0 {
		fmt.Println("Received malformed peers")
		return nil
	}
	infos := make([]PeerInfo, num)
	for i := 0; i < num; i++ {
		offset := i * PeerLen
		infos[i].Ip = net.IP(peers[offset : offset+IpLen])
		infos[i].Port = binary.BigEndian.Uint16(peers[offset+IpLen : offset+PeerLen])
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
func tryTracker(announceURL string, tf *TorrentFile, peerId [IDLEN]byte) []PeerInfo {
	// 临时替换Announce URL
	originalAnnounce := tf.Announce
	tf.Announce = announceURL
	defer func() { tf.Announce = originalAnnounce }()

	// 判断协议类型
	if strings.HasPrefix(announceURL, "udp://") {
		return tryUDPTracker(announceURL, tf, peerId)
	}

	// HTTP协议处理
	url, err := buildUrl(tf, peerId)
	if err != nil {
		fmt.Println("构建Tracker URL出错: " + err.Error())
		return nil
	}

	cli := &http.Client{Timeout: 15 * time.Second}
	resp, err := cli.Get(url)
	if err != nil {
		fmt.Println("无法连接到Tracker服务器: " + announceURL + " 错误: " + err.Error())
		return nil
	}
	defer resp.Body.Close()

	trackResp := new(TrackerResp)
	err = bencode.Unmarshal(resp.Body, trackResp)
	if err != nil {
		fmt.Println("Tracker响应解析错误: " + err.Error())
		return nil
	}

	return buildPeerInfo([]byte(trackResp.Peers))
}

// tryUDPTracker 尝试连接UDP协议的Tracker服务器
func tryUDPTracker(announceURL string, tf *TorrentFile, peerId [IDLEN]byte) []PeerInfo {
	// 解析UDP地址
	u, err := url.Parse(announceURL)
	if err != nil {
		fmt.Println("解析UDP地址错误: " + err.Error())
		return nil
	}

	// 连接UDP服务器
	conn, err := net.DialTimeout("udp", u.Host, 15*time.Second)
	if err != nil {
		fmt.Println("连接UDP Tracker失败: " + err.Error())
		return nil
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))

	// 构建UDP请求
	transactionID := uint32(time.Now().Unix())
	buf := make([]byte, 16)
	binary.BigEndian.PutUint64(buf[0:8], 0x41727101980) // connection_id
	binary.BigEndian.PutUint32(buf[8:12], 0)            // action=connect
	binary.BigEndian.PutUint32(buf[12:16], transactionID)

	// 发送连接请求
	_, err = conn.Write(buf)
	if err != nil {
		fmt.Println("发送UDP连接请求失败: " + err.Error())
		return nil
	}

	// 读取连接响应
	resp := make([]byte, 16)
	_, err = io.ReadFull(conn, resp)
	if err != nil {
		fmt.Println("读取UDP连接响应失败: " + err.Error())
		return nil
	}

	// 解析连接响应
	if binary.BigEndian.Uint32(resp[0:4]) != 0 || binary.BigEndian.Uint32(resp[4:8]) != transactionID {
		fmt.Println("UDP连接响应无效")
		return nil
	}
	connectionID := binary.BigEndian.Uint64(resp[8:16])

	// 构建announce请求
	announceBuf := make([]byte, 98)
	binary.BigEndian.PutUint64(announceBuf[0:8], connectionID)
	binary.BigEndian.PutUint32(announceBuf[8:12], 1) // action=announce
	binary.BigEndian.PutUint32(announceBuf[12:16], transactionID+1)
	copy(announceBuf[16:36], tf.InfoSHA[:])                          // info_hash
	copy(announceBuf[36:56], peerId[:])                              // peer_id
	binary.BigEndian.PutUint64(announceBuf[56:64], 0)                // downloaded
	binary.BigEndian.PutUint64(announceBuf[64:72], 0)                // left
	binary.BigEndian.PutUint64(announceBuf[72:80], 0)                // uploaded
	binary.BigEndian.PutUint32(announceBuf[80:84], 0)                // event
	binary.BigEndian.PutUint32(announceBuf[84:88], 0)                // IP address
	binary.BigEndian.PutUint32(announceBuf[88:92], 0)                // key
	binary.BigEndian.PutUint32(announceBuf[92:96], 50)               // num_want
	binary.BigEndian.PutUint16(announceBuf[96:98], uint16(PeerPort)) // port

	// 发送announce请求
	_, err = conn.Write(announceBuf)
	if err != nil {
		fmt.Println("发送UDP announce请求失败: " + err.Error())
		return nil
	}

	// 读取announce响应
	resp = make([]byte, 20+6*50) // 20字节头部 + 最多50个peer(每个6字节)
	n, err := io.ReadFull(conn, resp)
	if err != nil {
		fmt.Println("读取UDP announce响应失败: " + err.Error())
		return nil
	}

	// 解析announce响应
	if binary.BigEndian.Uint32(resp[0:4]) != 1 || binary.BigEndian.Uint32(resp[4:8]) != transactionID+1 {
		fmt.Println("UDP announce响应无效")
		return nil
	}

	// 解析peer列表
	peers := resp[20:n]
	return buildPeerInfo(peers)
}

func FindPeers(tf *TorrentFile, peerId [IDLEN]byte) []PeerInfo {
	// 获取所有Tracker服务器地址
	trackers := []string{tf.Announce}
	backupTrackers := readBackupTrackers(".\\tracker.txt")
	if backupTrackers != nil {
		trackers = append(trackers, backupTrackers...)
	}

	// 创建channel用于接收结果
	peerChan := make(chan []PeerInfo, len(trackers))
	var allPeers []PeerInfo

	// 并发请求所有Tracker服务器
	for _, tracker := range trackers {
		go func(url string) {
			peers := tryTracker(url, tf, peerId)
			if peers != nil {
				peerChan <- peers
			} else {
				peerChan <- nil
			}
		}(tracker)
	}

	// 收集并合并结果
	peerSet := make(map[string]PeerInfo)
	for i := 0; i < len(trackers); i++ {
		peers := <-peerChan
		if peers != nil {
			for _, peer := range peers {
				key := peer.Ip.String() + ":" + strconv.Itoa(int(peer.Port))
				if _, exists := peerSet[key]; !exists {
					peerSet[key] = peer
					allPeers = append(allPeers, peer)
				}
			}
		}
	}

	if len(allPeers) > 0 {
		fmt.Println("成功从", len(allPeers), "个Peer获取连接")
		return allPeers
	}

	fmt.Println("所有Tracker服务器均连接失败")
	return nil
}
