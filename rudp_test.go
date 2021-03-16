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
	var serverError, clientError error
	var serverAddress = "127.0.0.1:20000"
	var clientAddress = "127.0.0.1:30000"
	// server
	go func() {
		defer wait.Done()
		server, err := Listen(serverAddress)
		if err != nil {
			serverError = err
			return
		}
		// client
		go func() {
			defer wait.Done()
			client, err := Listen(clientAddress)
			if err != nil {
				clientError = err
				return
			}
			client.SetConnectRTO(time.Second * 3)
			conn, err := client.Dial(serverAddress, time.Hour)
			if err != nil {
				clientError = err
				return
			}
			buff := make([]byte, 1024)
			for i := 0; i < 1; i++ {
				mathRand.Read(buff)
				n, err := conn.Write(buff)
				if err != nil {
					clientError = err
					return
				}
				clientHash.Write(buff[:n])
			}
		}()
		buff := make([]byte, 1024)
		for {
			conn, err := server.Accept()
			if err != nil {
				serverError = err
				return
			}
			n, err := conn.Read(buff)
			if err != nil {
				if err != io.EOF {
					serverError = err
				}
				return
			}
			serverHash.Write(buff[:n])
		}
	}()
	wait.Wait()
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
