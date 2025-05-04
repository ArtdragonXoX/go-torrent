package torrent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

type MsgId uint8

const (
	MsgChoke       MsgId = 0
	MsgUnchoke     MsgId = 1
	MsgInterested  MsgId = 2
	MsgNotInterest MsgId = 3
	MsgHave        MsgId = 4
	MsgBitfield    MsgId = 5
	MsgRequest     MsgId = 6
	MsgPiece       MsgId = 7
	MsgCancel      MsgId = 8
	MsgDHTNodes    MsgId = 9  // 用于交换DHT节点的消息类型
	MsgPEX         MsgId = 10 // 用于交换Peer列表的消息类型
)

type PeerMsg struct {
	Id      MsgId
	Payload []byte
}

// NewDHTNodesMsg 创建一个用于交换DHT节点的消息
func NewDHTNodesMsg(nodesData []byte) *PeerMsg {
	return &PeerMsg{
		Id:      MsgDHTNodes,
		Payload: nodesData,
	}
}

// NewPEXMsg 创建一个用于交换Peer列表的消息
func NewPEXMsg(peersData []byte) *PeerMsg {
	return &PeerMsg{
		Id:      MsgPEX,
		Payload: peersData,
	}
}

// EncodePeers 将Peer列表编码为二进制格式
// 每个Peer占用6字节：4字节IP地址 + 2字节端口
func EncodePeers(peers []PeerInfo) []byte {
	if len(peers) == 0 {
		return nil
	}

	buf := make([]byte, len(peers)*6)
	for i, peer := range peers {
		offset := i * 6
		// 复制IP地址（4字节）
		copy(buf[offset:offset+4], peer.Ip.To4())
		// 复制端口（2字节）
		binary.BigEndian.PutUint16(buf[offset+4:offset+6], peer.Port)
	}
	return buf
}

// DecodePeers 将二进制格式解码为Peer列表
// 支持两种格式：
// 1. IPv4格式：每个peer占用6字节，前4字节为IPv4地址，后2字节为端口
// 2. IPv6格式：每个peer占用18字节，前16字节为IPv6地址，后2字节为端口
func DecodePeers(data []byte) []PeerInfo {
	// 检查输入数据
	if len(data) == 0 {
		fmt.Println("收到空的PEX数据")
		return nil
	}

	// 尝试判断是IPv4还是IPv6格式
	var isIPv6 bool
	var peerLen int

	// 检查数据长度是否符合IPv4或IPv6格式
	if len(data)%PeerV4Len == 0 {
		isIPv6 = false
		peerLen = PeerV4Len
		fmt.Printf("[DEBUG] 检测到IPv4格式的PEX数据\n")
	} else if len(data)%PeerV6Len == 0 {
		isIPv6 = true
		peerLen = PeerV6Len
		fmt.Printf("[DEBUG] 检测到IPv6格式的PEX数据\n")
	} else {
		// 尝试按照BEP-32规范检查是否为混合格式
		// 如果第一个字节是0x02，表示IPv6格式
		if len(data) > 0 && data[0] == 0x02 && (len(data)-1)%PeerV6Len == 0 {
			// 跳过第一个字节
			data = data[1:]
			isIPv6 = true
			peerLen = PeerV6Len
			fmt.Printf("[DEBUG] 检测到BEP-32格式的IPv6 PEX数据\n")
		} else {
			fmt.Printf("收到格式不正确的PEX数据: 长度=%d字节，既不是IPv4(%d的倍数)也不是IPv6(%d的倍数)\n",
				len(data), PeerV4Len, PeerV6Len)
			return nil
		}
	}

	peerCount := len(data) / peerLen
	peers := make([]PeerInfo, 0, peerCount)

	for i := 0; i < peerCount; i++ {
		offset := i * peerLen
		var ip net.IP
		var port uint16

		// 根据IP类型提取IP和端口
		if isIPv6 {
			// IPv6格式
			ip = net.IP(data[offset : offset+IpV6Len])
			port = binary.BigEndian.Uint16(data[offset+IpV6Len : offset+PeerV6Len])
		} else {
			// IPv4格式
			ip = net.IP(data[offset : offset+IpV4Len])
			port = binary.BigEndian.Uint16(data[offset+IpV4Len : offset+PeerV4Len])
		}

		// 创建PeerInfo并添加到列表
		peers = append(peers, PeerInfo{
			Ip:       ip,
			Port:     port,
			LastSeen: time.Now(),
		})
	}
	return peers
}

type PeerConn struct {
	net.Conn
	Choked  bool
	Field   Bitfield
	peer    PeerInfo
	peerId  [IDLEN]byte
	infoSHA [SHALEN]byte
	task    *TorrentTask // 添加对TorrentTask的引用
}

func handshake(conn net.Conn, infoSHA [SHALEN]byte, peerId [IDLEN]byte) error {
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetDeadline(time.Time{})
	// send HandshakeMsg
	req := NewHandShakeMsg(infoSHA, peerId)
	_, err := WriteHandShake(conn, req)
	if err != nil {
		// fmt.Println("send handshake failed")
		return err
	}
	// read HandshakeMsg
	res, err := ReadHandshake(conn)
	if err != nil {
		// fmt.Println("read handshake failed")
		return err
	}
	// check HandshakeMsg
	if !bytes.Equal(res.InfoSHA[:], infoSHA[:]) {
		// fmt.Println("check handshake failed")
		return fmt.Errorf("handshake msg error: " + string(res.InfoSHA[:]))
	}
	return nil
}

func fillBitfield(c *PeerConn) error {
	c.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.SetDeadline(time.Time{})

	msg, err := c.ReadMsg()
	if err != nil {
		return err
	}
	if msg == nil {
		return fmt.Errorf("expected bitfield")
	}
	if msg.Id != MsgBitfield {
		return fmt.Errorf("expected bitfield, get " + strconv.Itoa(int(msg.Id)))
	}
	fmt.Println("fill bitfield : " + c.peer.Ip.String())
	c.Field = msg.Payload
	return nil
}

func (c *PeerConn) ReadMsg() (*PeerMsg, error) {
	// read msg length
	lenBuf := make([]byte, 4)
	_, err := io.ReadFull(c, lenBuf)
	if err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(lenBuf)
	// keep alive msg
	if length == 0 {
		return nil, nil
	}
	// read msg body
	msgBuf := make([]byte, length)
	_, err = io.ReadFull(c, msgBuf)
	if err != nil {
		return nil, err
	}

	msg := &PeerMsg{
		Id:      MsgId(msgBuf[0]),
		Payload: msgBuf[1:],
	}

	peerKey := fmt.Sprintf("%s:%d", c.peer.Ip.String(), c.peer.Port)

	// 处理DHT节点消息
	if msg.Id == MsgDHTNodes && c.task != nil && c.task.DHT != nil {
		// 如果是空的DHT节点请求（payload为空），则自动回复我们的DHT节点
		if len(msg.Payload) == 0 {
			// 这是一个DHT节点请求，自动回复我们的DHT节点
			fmt.Printf("收到来自Peer %s 的DHT节点请求，准备回复\n", peerKey)

			// 编码本地DHT节点并发送给peer
			nodesData := c.task.DHT.EncodeNodes()
			if len(nodesData) > 0 {
				dhtMsg := NewDHTNodesMsg(nodesData)
				_, err := c.WriteMsg(dhtMsg)
				if err != nil {
					fmt.Printf("向Peer %s 回复DHT节点信息失败: %v\n", peerKey, err)
				} else {
					fmt.Printf("向Peer %s 回复了DHT节点信息\n", peerKey)
				}
			}
		}
	}

	// 处理PEX消息
	if msg.Id == MsgPEX && c.task != nil {
		// 如果是空的PEX请求（payload为空），则自动回复我们的Peer列表
		if len(msg.Payload) == 0 {
			fmt.Printf("收到来自Peer %s 的PEX请求，准备回复\n", peerKey)

			// 获取当前已知的Peer列表（排除当前连接的Peer）
			c.task.peerMu.RLock()
			var peersToShare []PeerInfo
			for _, p := range c.task.PeerList {
				// 不分享当前连接的Peer自身
				if p.Ip.String() != c.peer.Ip.String() || p.Port != c.peer.Port {
					peersToShare = append(peersToShare, p)
				}
			}
			c.task.peerMu.RUnlock()

			// 限制分享的Peer数量，避免消息过大
			if len(peersToShare) > 50 {
				peersToShare = peersToShare[:50]
			}

			// 编码Peer列表并发送
			peersData := EncodePeers(peersToShare)
			if len(peersData) > 0 {
				pexMsg := NewPEXMsg(peersData)
				_, err := c.WriteMsg(pexMsg)
				if err != nil {
					fmt.Printf("向Peer %s 回复PEX信息失败: %v\n", peerKey, err)
				} else {
					fmt.Printf("向Peer %s 回复了PEX信息，分享了%d个Peer\n", peerKey, len(peersToShare))
				}
			}
		} else {
			// 处理收到的PEX数据
			peers := DecodePeers(msg.Payload)
			if len(peers) > 0 {
				fmt.Printf("从Peer %s 收到PEX数据，包含%d个Peer\n", peerKey, len(peers))
				// 添加到任务的Peer列表
				c.task.addPeers(peers)
			}
		}
	}

	return msg, nil
}

const LenBytes uint32 = 4

func (c *PeerConn) WriteMsg(m *PeerMsg) (int, error) {
	var buf []byte
	if m == nil {
		buf = make([]byte, LenBytes)
	}
	length := uint32(len(m.Payload) + 1) // +1 for id
	buf = make([]byte, LenBytes+length)
	binary.BigEndian.PutUint32(buf[0:LenBytes], length)
	buf[LenBytes] = byte(m.Id)
	copy(buf[LenBytes+1:], m.Payload)
	return c.Write(buf)
}

func CopyPieceData(index int, buf []byte, msg *PeerMsg) (int, error) {
	if msg.Id != MsgPiece {
		return 0, fmt.Errorf("expected MsgPiece (Id %d), got Id %d", MsgPiece, msg.Id)
	}
	if len(msg.Payload) < 8 {
		return 0, fmt.Errorf("payload too short. %d < 8", len(msg.Payload))
	}
	parsedIndex := int(binary.BigEndian.Uint32(msg.Payload[0:4]))
	if parsedIndex != index {
		return 0, fmt.Errorf("expected index %d, got %d", index, parsedIndex)
	}
	offset := int(binary.BigEndian.Uint32(msg.Payload[4:8]))
	if offset >= len(buf) {
		return 0, fmt.Errorf("offset too high. %d >= %d", offset, len(buf))
	}
	data := msg.Payload[8:]
	if offset+len(data) > len(buf) {
		return 0, fmt.Errorf("data too large [%d] for offset %d with length %d", len(data), offset, len(buf))
	}
	copy(buf[offset:], data)
	return len(data), nil
}

func GetHaveIndex(msg *PeerMsg) (int, error) {
	if msg.Id != MsgHave {
		return 0, fmt.Errorf("expected MsgHave (Id %d), got Id %d", MsgHave, msg.Id)
	}
	if len(msg.Payload) != 4 {
		return 0, fmt.Errorf("expected payload length 4, got length %d", len(msg.Payload))
	}
	index := int(binary.BigEndian.Uint32(msg.Payload))
	return index, nil
}

func NewRequestMsg(index, offset, length int) *PeerMsg {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], uint32(index))
	binary.BigEndian.PutUint32(payload[4:8], uint32(offset))
	binary.BigEndian.PutUint32(payload[8:12], uint32(length))
	return &PeerMsg{MsgRequest, payload}
}

func NewConn(peer PeerInfo, infoSHA [SHALEN]byte, peerId [IDLEN]byte, task *TorrentTask) (*PeerConn, error) {
	// setup tcp conn
	addr := net.JoinHostPort(peer.Ip.String(), strconv.Itoa(int(peer.Port)))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		// fmt.Println("set tcp conn failed: " + addr)
		return nil, err
	}
	// torrent p2p handshake
	err = handshake(conn, infoSHA, peerId)
	if err != nil {
		// fmt.Println("handshake failed")
		conn.Close()
		return nil, err
	}
	c := &PeerConn{
		Conn:    conn,
		Choked:  true,
		peer:    peer,
		peerId:  peerId,
		infoSHA: infoSHA,
		task:    task,
	}
	// fill bitfield
	err = fillBitfield(c)
	if err != nil {
		// fmt.Println("fill bitfield failed, " + err.Error())
		return nil, err
	}
	return c, nil
}
