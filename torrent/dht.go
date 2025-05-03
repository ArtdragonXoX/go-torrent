package torrent

import (
	"bt_download/bencode"
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"
)

const (
	KeySize        = 20
	MaxPacketSize  = 8192
	DefaultDhtPort = 6881

	// DHT消息类型
	MsgDHTNodes = 9 // 用于交换DHT节点的消息类型
)

type NodeID [KeySize]byte

type Contact struct {
	ID   NodeID
	IP   net.IP
	Port uint16
}

type DHT struct {
	conn     *net.UDPConn
	self     Contact
	peers    map[NodeID][]PeerInfo
	peersMu  sync.RWMutex
	nodes    map[NodeID]Contact
	nodesMu  sync.RWMutex
	quit     chan struct{}
	txid     uint16
	txidMu   sync.Mutex
	waitChan map[string]chan []PeerInfo
	waitMu   sync.Mutex
}

type KRPC struct {
	T string                 `bencode:"t"`
	Y string                 `bencode:"y"`
	Q string                 `bencode:"q,omitempty"`
	A map[string]interface{} `bencode:"a,omitempty"`
	R map[string]interface{} `bencode:"r,omitempty"`
}

func NewDHT(port int) (*DHT, error) {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	// Generate random node ID
	var selfID NodeID
	rand.Read(selfID[:])

	dht := &DHT{
		conn:     conn,
		self:     Contact{ID: selfID, IP: net.IPv4zero, Port: uint16(port)},
		peers:    make(map[NodeID][]PeerInfo),
		nodes:    make(map[NodeID]Contact),
		quit:     make(chan struct{}),
		waitChan: make(map[string]chan []PeerInfo),
	}

	go dht.listen()
	go dht.bootstrap()
	return dht, nil
}

func (dht *DHT) bootstrap() {
	// Add some well-known bootstrap nodes
	bootstraps := []string{
		"router.bittorrent.com:6881",
		"dht.transmissionbt.com:6881",
		"router.utorrent.com:6881",
	}

	for _, addr := range bootstraps {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			continue
		}
		p, _ := net.LookupPort("udp", port)
		dht.nodesMu.Lock()
		dht.nodes[NodeID{}] = Contact{
			IP:   ips[0],
			Port: uint16(p),
		}
		dht.nodesMu.Unlock()
	}
}

func (dht *DHT) listen() {
	buf := make([]byte, MaxPacketSize)
	for {
		select {
		case <-dht.quit:
			return
		default:
			n, addr, err := dht.conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			go dht.handlePacket(buf[:n], addr)
		}
	}
}

func (dht *DHT) handlePacket(data []byte, addr *net.UDPAddr) {
	var msg KRPC
	buf := bytes.NewReader(data)
	if err := bencode.Unmarshal(buf, &msg); err != nil {
		return
	}

	switch msg.Y {
	case "q":
		dht.handleQuery(msg, addr)
	case "r":
		dht.handleResponse(msg, addr)
	}
}

func (dht *DHT) GetPeers(infoHash [KeySize]byte) []PeerInfo {
	// Return cached peers
	dht.peersMu.RLock()
	peers := dht.peers[infoHash]
	dht.peersMu.RUnlock()

	// If not enough peers, query DHT
	if len(peers) < 100 {
		newPeers := dht.findPeers(infoHash)
		fmt.Println("Find peers:", newPeers)
		if len(newPeers) > 0 {
			dht.updatePeers(infoHash, newPeers)
			peers = append(peers, newPeers...)
		}
	}
	return peers
}

func (dht *DHT) findPeers(infoHash [KeySize]byte) []PeerInfo {
	txid := dht.nextTxID()
	waitKey := fmt.Sprintf("%x-%d", infoHash, txid)
	ch := make(chan []PeerInfo, 1)

	dht.waitMu.Lock()
	dht.waitChan[waitKey] = ch
	dht.waitMu.Unlock()

	// Send get_peers query to closest nodes
	nodes := dht.findClosestNodes(infoHash, 8)
	for _, node := range nodes {
		msg := KRPC{
			T: fmt.Sprintf("%d", txid),
			Y: "q",
			Q: "get_peers",
			A: map[string]interface{}{
				"id":        dht.self.ID,
				"info_hash": infoHash[:],
			},
		}
		dht.sendQuery(msg, node)
	}

	// Wait for response or timeout
	select {
	case peers := <-ch:
		return peers
	case <-time.After(5 * time.Second):
		return nil
	}
}

func (dht *DHT) nextTxID() uint16 {
	dht.txidMu.Lock()
	defer dht.txidMu.Unlock()
	dht.txid++
	return dht.txid
}

func (dht *DHT) findClosestNodes(target NodeID, count int) []Contact {
	var nodes []Contact
	dht.nodesMu.RLock()
	for _, node := range dht.nodes {
		nodes = append(nodes, node)
		if len(nodes) >= count {
			break
		}
	}
	dht.nodesMu.RUnlock()
	return nodes
}

func (dht *DHT) sendQuery(msg KRPC, node Contact) error {
	addr := &net.UDPAddr{
		IP:   node.IP,
		Port: int(node.Port),
	}
	return dht.sendResponse(msg, addr)
}

func (dht *DHT) handleQuery(msg KRPC, addr *net.UDPAddr) {
	switch msg.Q {
	case "get_peers":
		dht.handleGetPeersQuery(msg, addr)
	case "announce_peer":
		dht.handleAnnounceQuery(msg, addr)
	}
}

func (dht *DHT) handleResponse(msg KRPC, addr *net.UDPAddr) {
	if values, ok := msg.R["values"].([]interface{}); ok {
		var hash [KeySize]byte
		if infoHash, ok := msg.R["info_hash"].([]byte); ok {
			copy(hash[:], infoHash)
			dht.updatePeers(hash, decodePeers(values))
		}
	}
}

func (dht *DHT) handleGetPeersQuery(msg KRPC, addr *net.UDPAddr) {
	infoHash, ok := msg.A["info_hash"].([]byte)
	if !ok || len(infoHash) != KeySize {
		return
	}

	var hash [KeySize]byte
	copy(hash[:], infoHash)

	peers := dht.GetPeers(hash)
	if len(peers) > 0 {
		response := KRPC{
			T: msg.T,
			Y: "r",
			R: map[string]interface{}{
				"id":     msg.A["id"],
				"values": encodePeers(peers),
			},
		}
		dht.sendResponse(response, addr)
	}
}

func (dht *DHT) handleAnnounceQuery(msg KRPC, addr *net.UDPAddr) {
	infoHash, ok1 := msg.A["info_hash"].([]byte)
	token, ok2 := msg.A["token"].(string)
	port, ok3 := msg.A["port"].(int64)
	if !ok1 || !ok2 || !ok3 || len(infoHash) != KeySize {
		return
	}

	if !dht.verifyToken(token, addr) {
		return
	}

	var hash [KeySize]byte
	copy(hash[:], infoHash)

	peer := PeerInfo{
		Ip:   addr.IP,
		Port: uint16(port),
	}

	dht.peersMu.Lock()
	dht.peers[hash] = append(dht.peers[hash], peer)
	dht.peersMu.Unlock()

	response := KRPC{
		T: msg.T,
		Y: "r",
		R: map[string]interface{}{
			"id": msg.A["id"],
		},
	}
	dht.sendResponse(response, addr)
}

func (dht *DHT) sendResponse(msg KRPC, addr *net.UDPAddr) error {
	var buf bytes.Buffer
	bencode.Marshal(&buf, msg)
	_, err := dht.conn.WriteToUDP(buf.Bytes(), addr)
	return err
}

func (dht *DHT) updatePeers(infoHash [KeySize]byte, peers []PeerInfo) {
	dht.peersMu.Lock()
	defer dht.peersMu.Unlock()
	dht.peers[infoHash] = append(dht.peers[infoHash], peers...)
}

func (dht *DHT) generateToken(addr *net.UDPAddr) string {
	secret := make([]byte, KeySize)
	rand.Read(secret)
	h := hmac.New(sha1.New, secret)
	h.Write(addr.IP.To4())
	binary.Write(h, binary.BigEndian, uint16(addr.Port))
	return string(h.Sum(nil)[:8])
}

func (dht *DHT) verifyToken(token string, addr *net.UDPAddr) bool {
	expected := dht.generateToken(addr)
	return hmac.Equal([]byte(token), []byte(expected))
}

// AddNodesFromPeer 从已连接的peer获取DHT节点信息
func (dht *DHT) AddNodesFromPeer(nodes []Contact) {
	if dht == nil {
		return
	}

	dht.nodesMu.Lock()
	defer dht.nodesMu.Unlock()

	// 添加新节点到DHT网络
	for _, node := range nodes {
		// 跳过无效节点
		if node.IP == nil || node.Port == 0 {
			continue
		}

		// 添加到节点列表
		dht.nodes[node.ID] = node
		fmt.Printf("从Peer添加DHT节点: %s:%d\n", node.IP.String(), node.Port)
	}
}

// EncodeNodes 将DHT节点编码为可传输的格式
func (dht *DHT) EncodeNodes() []byte {
	if dht == nil {
		return nil
	}

	dht.nodesMu.RLock()
	defer dht.nodesMu.RUnlock()

	// 最多返回20个节点
	count := 0
	var buf bytes.Buffer

	for _, node := range dht.nodes {
		if count >= 20 {
			break
		}

		// 编码节点ID (20字节)
		buf.Write(node.ID[:])

		// 编码IP (4字节)
		buf.Write(node.IP.To4())

		// 编码端口 (2字节)
		binary.Write(&buf, binary.BigEndian, node.Port)

		count++
	}

	return buf.Bytes()
}

// DecodeNodes 解码从peer接收到的DHT节点信息
func DecodeNodes(data []byte) []Contact {
	if len(data) == 0 || len(data)%26 != 0 { // 每个节点26字节(20+4+2)
		return nil
	}

	var nodes []Contact
	for i := 0; i < len(data); i += 26 {
		if i+26 > len(data) {
			break
		}

		var node Contact

		// 解码节点ID
		copy(node.ID[:], data[i:i+20])

		// 解码IP
		node.IP = net.IPv4(data[i+20], data[i+21], data[i+22], data[i+23])

		// 解码端口
		node.Port = binary.BigEndian.Uint16(data[i+24 : i+26])

		nodes = append(nodes, node)
	}

	return nodes
}

func (dht *DHT) Stop() {
	close(dht.quit)
	dht.conn.Close()
}

func decodePeers(values []interface{}) []PeerInfo {
	var peers []PeerInfo
	for _, v := range values {
		if peerStr, ok := v.(string); ok && len(peerStr) == 6 {
			peers = append(peers, PeerInfo{
				Ip:   net.IP(peerStr[:4]),
				Port: binary.BigEndian.Uint16([]byte(peerStr[4:6])),
			})
		}
	}
	return peers
}

func encodePeers(peers []PeerInfo) []string {
	var result []string
	for _, peer := range peers {
		var buf bytes.Buffer
		buf.Write(peer.Ip.To4())
		binary.Write(&buf, binary.BigEndian, peer.Port)
		result = append(result, buf.String())
	}
	return result
}
