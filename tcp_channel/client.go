package main

import (
	"flag"
	"fmt"
	"github.com/qq51529210/rudp"
	"io"
	"net"
	"time"
)

func runClient() {
	cmd := flag.NewFlagSet("client", flag.PanicOnError)
	listen := cmd.String("listen", "0.0.0.0:10000", "tcp_channel client tcp listen address")
	server := cmd.String("server", "", "tcp_channel server udp listen address")
	// 初始化
	rudp.DefaultConfig.Listen = *listen
	client, err := rudp.NewWithConfig(&rudp.DefaultConfig)
	if err != nil {
		panic(err)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		panic(err)
	}
	// 监听
	for {
		// 等待新tcp连接
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println(err)
			return
		}
		// 把数据转发到proxy
		go func(c1 net.Conn) {
			defer c1.Close()
			// 连接到proxy服务
			c2, err := client.Dial(*server, time.Second*5)
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
