package main

import (
	"flag"
	"fmt"
	"github.com/qq51529210/rudp"
	"io"
	"net"
)

func runServer() {
	cmd := flag.NewFlagSet("server", flag.PanicOnError)
	listen := cmd.String("listen", "0.0.0.0:10000", "tcp_channel server udp listen address")
	proxy := cmd.String("proxy", "", "proxy tcp listen address")
	// 初始化
	rudp.DefaultConfig.Listen = *listen
	server, err := rudp.NewWithConfig(&rudp.DefaultConfig)
	if err != nil {
		panic(err)
	}
	// 监听
	for {
		// 等待新连接
		conn, err := server.Accept()
		if err != nil {
			fmt.Println(err)
			return
		}
		// 把数据转发到proxy
		go func(c1 net.Conn) {
			defer c1.Close()
			// 连接到proxy服务
			c2, err := net.Dial("tcp", *proxy)
			if err != nil {
				fmt.Println(err)
				return
			}
			io.Copy(c2, c1)
			// proxy数据转发到client
			go func(c1, c2 net.Conn) {
				defer c2.Close()
				io.Copy(c1, c2)
			}(c1, c2)
		}(conn)
	}
}
