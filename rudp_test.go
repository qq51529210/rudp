package rudp

import (
	"bytes"
	"crypto/md5"
	"io"
	"sync"
	"testing"
	"time"
)

func Test_RUDP(t *testing.T) {
	wait := sync.WaitGroup{}
	wait.Add(2)
	// server
	server, err := Listen("127.0.0.1:10000")
	if err != nil {
		t.Fatal(err)
	}
	// client
	client, err := Listen("127.0.0.1:10000")
	if err != nil {
		t.Fatal(err)
	}
	// 哈希，用于校验传输的数据
	wh := md5.New()
	rh := md5.New()
	// server协程
	go func() {
		defer wait.Done()
		// 监听新的Conn
		conn, err := server.Accept()
		if err != nil {
			t.Log(err)
			return
		}
		//t.Log("conn from", conn.RemoteAddr())
		var buf [maxMSS]byte
		for {
			// 读取数据
			n, err := conn.Read(buf[:])
			if err != nil {
				// 判断对方是否关闭连接
				if err != io.EOF {
					t.Log(err)
					return
				} else {
					// 对方关闭，退出循环
					break
				}
			}
			// 写入哈希
			rh.Write(buf[:n])
		}
		conn.Close()
	}()
	// client协程
	go func() {
		defer wait.Done()
		// 拨号连接
		conn, err := client.Dial("127.0.0.1:10000", time.Second*10)
		if err != nil {
			t.Log(err)
			return
		}
		//t.Log("conn from", conn.RemoteAddr())
		buf := make([]byte, 1024)
		n := 10240
		// 1024*10240，10m的数据
		for i := 0; i < n; i++ {
			// 随机数据
			mathRand.Read(buf)
			// 写入哈希
			wh.Write(buf)
			// 发送
			_, err = conn.Write(buf)
			if err != nil {
				t.Log(err)
				return
			}
		}
		// 关闭连接
		conn.Close()
	}()
	// 等待完成
	wait.Wait()
	server.Close()
	client.Close()
	// 比较传输的数据哈希值
	if !bytes.Equal(wh.Sum(nil), rh.Sum(nil)) {
		t.FailNow()
	}
}
