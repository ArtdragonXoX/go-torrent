package test

import (
	"bt_download/torrent"
	"bufio"
	"os"
	"testing"
)

func TestParse(t *testing.T) {
	file, err := os.Open("../../alpine-standard-3.21.3-x86_64.iso.torrent")
	if err != nil {
		t.Error("open file error")
		return
	}
	defer file.Close()
	tf, err := torrent.ParseFile(bufio.NewReader(file))
	if err != nil {
		t.Error("parse file error")
		return
	}
	t.Log(tf)
}
