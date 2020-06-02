package rudp

import (
	"bytes"
	"crypto/md5"
	"io"
	"sync"
	"testing"
	"time"
)

func Test_RUDP_Dial_Accept(t *testing.T) {
	wait := sync.WaitGroup{}
	wait.Add(2)
	DefaultConfig.Listen = "127.0.0.1:10000"
	server, err := NewWithConfig(&DefaultConfig)
	if err != nil {
		t.Fatal(err)
	}
	DefaultConfig.Listen = "127.0.0.1:20000"
	client, err := NewWithConfig(&DefaultConfig)
	if err != nil {
		t.Fatal(err)
	}

	wh := md5.New()
	rh := md5.New()

	go func() {
		defer wait.Done()
		conn, err := server.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Log("conn from", conn.RemoteAddr())
		var buf udpBuf
		for {
			n, err := conn.Read(buf[:])
			if err != nil {
				if err != io.EOF {
					t.Fatal(err)
				} else {
					break
				}
			}
			rh.Write(buf[:n])
		}
		conn.Close()
	}()
	go func() {
		defer wait.Done()
		conn, err := client.Dial("127.0.0.1:10000", time.Second*10)
		if err != nil {
			t.Fatal(err)
		}
		t.Log("conn from", conn.RemoteAddr())
		buf := make([]byte, 1024)
		for i := 0; i < 1000; i++ {
			// 随机数据
			_rand.Read(buf)
			// 哈希发送的数据
			wh.Write(buf)
			_, err = conn.Write(buf)
			if err != nil {
				t.Fatal(err)
			}
		}
		conn.Close()
	}()

	wait.Wait()
	server.Close()
	client.Close()

	if !bytes.Equal(wh.Sum(nil), rh.Sum(nil)) {
		t.FailNow()
	}
}

func Test_RUDP_Close(t *testing.T) {
	DefaultConfig.Listen = "127.0.0.1:10000"
	rudp, err := NewWithConfig(&DefaultConfig)
	if err != nil {
		t.Fatal(err)
	}
	w := sync.WaitGroup{}
	n := 1000
	for i := 0; i < 5; i++ {
		w.Add(1)
		go func() {
			for i := 0; i < n; i++ {
				rudp.Close()
			}
			w.Done()
		}()
	}
	w.Wait()
}
