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
			}
			t.Log(string(buf[:n]))
			n, err = conn.Write([]byte("server say hello"))
			if err != nil {
				t.Fatal(err)
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
		_, err = conn.Write([]byte("client say hello"))
		if err != nil {
			t.Fatal(err)
		}
		var buf udpBuf
		n, err := conn.Read(buf[:])
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(buf[:n]))
		conn.Close()
	}()
	wait.Wait()
	server.Close()
	client.Close()
}

func Test_RUDP_Close(t *testing.T) {
	rudp, err := New("127.0.0.1:10000")
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
