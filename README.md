# Go-Torrent 下载器

## 项目介绍

本项目是一个基于Go语言实现的BitTorrent下载客户端，基于[archeryue/go-torrent](https://github.com/archeryue/go-torrent)修改。项目实现了BitTorrent协议的核心功能，能够解析torrent文件并从网络中下载相应的文件。

### 主要改进

- 调整了项目结构，使其更加扁平化，便于维护和扩展
- 重构了tracker模块，使其更加清晰和易于扩展，增加了udp、ipv6的支持，增加了对备用tracker的支持
- 增加了PeerManager模块，用于集中管理Peer连接和下载任务
- 支持HTTP和UDP两种Tracker协议
- 增加了日志记录功能

## 功能特点

- **Torrent文件解析**：支持解析标准的.torrent文件
- **Tracker通信**：支持与HTTP/HTTPS和UDP类型的Tracker服务器通信
- **P2P下载**：实现了BitTorrent协议的P2P下载功能
- **Peer管理**：通过PeerManager集中管理多个Peer连接
- **数据校验**：使用SHA1算法验证下载数据的完整性

## 使用方法

### 编译

```bash
go build -o bt_download.exe
```

### 运行

```bash
# 基本用法
bt_download.exe <torrent文件路径>

# 启用日志记录
bt_download.exe <torrent文件路径> -log
```

## 项目结构

```
.
├── main.go                 # 程序入口
├── torrent/               # 核心功能模块
│   ├── bitfield.go        # 位图实现，用于记录已下载的piece
│   ├── download.go        # 下载逻辑实现
│   ├── handshake.go       # BitTorrent握手协议实现
│   ├── log.go             # 日志功能
│   ├── peer.go            # Peer连接管理
│   ├── peer_manger.go     # Peer管理器，管理多个Peer连接
│   ├── torrent_file.go    # Torrent文件解析
│   └── tracker.go         # Tracker通信实现
```

## 核心模块说明

### TorrentFile

负责解析.torrent文件，提取文件信息、Tracker地址和分片SHA1哈希值等。

### Tracker

负责与Tracker服务器通信，获取Peer列表。支持HTTP/HTTPS和UDP两种协议。

### PeerManager

管理多个Peer连接，分配下载任务，收集下载结果。主要功能包括：
- 维护活跃Peer列表
- 为每个Peer分配下载任务
- 管理Peer连接的生命周期
- 收集下载结果并组装文件

### Peer

实现与单个Peer的通信，包括握手、消息交换和数据传输。

## 协议实现

项目实现了BitTorrent协议的核心部分，包括：

- **握手协议**：与Peer建立连接
- **消息协议**：包括choke、unchoke、interested、not interested、have、bitfield、request、piece、cancel等消息类型
- **分片下载**：将文件分割成多个piece进行并行下载
- **数据校验**：使用SHA1哈希验证下载数据的完整性

## 注意事项

- 本项目仅用于学习BitTorrent协议，请勿用于下载非法内容
- 下载速度受网络环境和可用Peer数量影响
- 大文件下载可能需要较长时间

