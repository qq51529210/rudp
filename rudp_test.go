package rudp

import (
	"io"
	"sync"
	"testing"
	"time"
)

func Test_RUDP_Dial_Accept(t *testing.T) {
	wait := sync.WaitGroup{}
	wait.Add(2)
	server, err := New("127.0.0.1:10000")
	if err != nil {
		t.Fatal(err)
	}
	client, err := New("127.0.0.1:20000")
	if err != nil {
		t.Fatal(err)
	}
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
			} else {
				t.Log(string(buf[:n]))
			}
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
		_, err = conn.Write([]byte("hello"))
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}()
	wait.Wait()
	server.Close()
	client.Close()
}
