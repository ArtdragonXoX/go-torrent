package torrent

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"os"
	"time"
)

type TorrentTask struct {
	InfoSHA  [SHALEN]byte
	FileName string
	FileLen  int
	PieceLen int
	PieceSHA [][SHALEN]byte
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

const BLOCKSIZE = 16384
const MAXBACKLOG = 5

func (state *taskState) handleMsg() error {
	msg, err := state.conn.ReadMsg()
	if err != nil {
		return err
	}
	// handle keep-alive
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
	}
	return nil
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

func (t *TorrentTask) getPieceBounds(index int) (bengin, end int) {
	bengin = index * t.PieceLen
	end = bengin + t.PieceLen
	if end > t.FileLen {
		end = t.FileLen
	}
	return
}

func (t *TorrentFile) getPieceBounds(index int) (bengin, end int) {
	bengin = index * t.PieceLen
	end = bengin + t.PieceLen
	if end > t.FileLen {
		end = t.FileLen
	}
	return
}

func Download(peerId [20]byte, tf *TorrentFile) error {
	resultQueue := make(chan *pieceResult)
	pm := NewPeerManager(peerId, tf, resultQueue)
	pm.Start()
	// collect piece result
	buf := make([]byte, tf.FileLen)
	count := 0
	for count < len(tf.PieceSHA) {
		res := <-resultQueue
		begin, end := tf.getPieceBounds(res.index)
		copy(buf[begin:end], res.data)
		count++
		// print progress
		percent := float64(count) / float64(len(tf.PieceSHA)) * 100
		fmt.Printf("downloading, progress : (%0.2f%%)\n", percent)
	}
	close(resultQueue)
	// create file & copy data
	file, err := os.Create(tf.FileName)
	if err != nil {
		fmt.Println("fail to create file: " + tf.FileName)
		return err
	}
	_, err = file.Write(buf)
	if err != nil {
		fmt.Println("fail to write data")
		return err
	}
	return nil
}
