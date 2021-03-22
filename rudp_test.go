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
	clientHash := md5.New()
	serverHash := md5.New()
	var multiple = 20
	var serverError, clientError error
	var serverBytes, clientBytes int
	var serverAddress = "127.0.0.1:20000"
	var clientAddress = "127.0.0.1:30000"
	server, err := Listen(serverAddress)
	if err != nil {
		serverError = err
		return
	}
	defer server.Close()
	client, err := Listen(clientAddress)
	if err != nil {
		clientError = err
		return
	}
	defer client.Close()
	// server
	go func() {
		defer wait.Done()
		buff := make([]byte, 1024)
		conn, err := server.Accept()
		if err != nil {
			serverError = err
			return
		}
		for {
			n, err := conn.Read(buff)
			if err != nil {
				if err != io.EOF {
					serverError = err
				}
				return
			}
			serverBytes += n
			serverHash.Write(buff[:n])
		}
	}()
	// client
	go func() {
		// 等server accept
		time.Sleep(time.Millisecond * 300)
		defer wait.Done()
		client.SetConnectRTO(time.Second * 3)
		conn, err := client.Dial(serverAddress, time.Hour)
		if err != nil {
			clientError = err
			return
		}
		conn.SetWriteBuffer(minMSS * 10)
		buff := make([]byte, 1024)
		for i := 0; i < multiple; i++ {
			mathRand.Read(buff)
			n, err := conn.Write(buff)
			if err != nil {
				clientError = err
				return
			}
			clientBytes += n
			clientHash.Write(buff[:n])
		}
		conn.Close()
	}()
	wait.Wait()
	t.Log(serverBytes, clientBytes)
	if serverError != nil {
		t.Fatal(serverError)
	}
	if clientError != nil {
		t.Fatal(clientError)
	}
	// 比较传输的数据哈希值
	if !bytes.Equal(clientHash.Sum(nil), serverHash.Sum(nil)) {
		t.FailNow()
	}
}
