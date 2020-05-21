package rudp

import (
	"sync"
	"testing"
	"time"
)

func Test_RUDP_Dial_Accept(t *testing.T) {
	wait := sync.WaitGroup{}
	wait.Add(2)
	go func() {
		defer wait.Done()
		server, err := New("127.0.0.1:10000")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := server.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Log("conn from", conn.RemoteAddr())
		conn.Close()
		server.Close()
	}()
	go func() {
		defer wait.Done()
		client, err := New("127.0.0.1:20000")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := client.Dial("127.0.0.1:10000", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Log("conn from", conn.RemoteAddr())
		conn.Close()
		client.Close()
	}()
	wait.Wait()
}
