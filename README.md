# rudp
一个基于udp协议的可靠性传输库，主要的结构有
1. RUDP，实现了net.Listener接口，和一个Dial函数
2. Conn，实现了net.Conn接口  
## 用法 
代码在rudp_test.go中
```
func Test_RUDP_IO(t *testing.T) {
	wait := sync.WaitGroup{}
	wait.Add(2)
	// server
	DefaultConfig.Listen = "127.0.0.1:10000"
	server, err := NewWithConfig(&DefaultConfig)
	if err != nil {
		t.Fatal(err)
	}
	// client
	DefaultConfig.Listen = "127.0.0.1:20000"
	client, err := NewWithConfig(&DefaultConfig)
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
			t.Fatal(err)
		}
		//t.Log("conn from", conn.RemoteAddr())
		var buf udpBuf
		for {
			// 读取数据
			n, err := conn.Read(buf[:])
			if err != nil {
				// 判断对方是否关闭连接
				if err != io.EOF {
					t.Fatal(err)
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
			t.Fatal(err)
		}
		//t.Log("conn from", conn.RemoteAddr())
		buf := make([]byte, 1024)
		n := 10240
		// 1024*10240，10m的数据
		for i := 0; i < n; i++ {
			// 随机数据
			_rand.Read(buf)
			// 写入哈希
			wh.Write(buf)
			// 发送
			_, err = conn.Write(buf)
			if err != nil {
				t.Fatal(err)
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
```
## tcp_channel
使用rudp实现的一个加速tcp传输的程序 
