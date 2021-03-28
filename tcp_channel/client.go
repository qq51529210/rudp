package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/qq51529210/rudp"
)

func runClient() {
	cmd := flag.NewFlagSet("client", flag.PanicOnError)
	listen := cmd.String("listen", "0.0.0.0:10000", "tcp_channel client tcp listen address")
	server := cmd.String("server", "", "tcp_channel server udp listen address")
	// 初始化
	client, err := rudp.Listen(*listen)
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
			tlsConn := tls.Client(c2, &tls.Config{
				InsecureSkipVerify: true,
			})
			io.Copy(tlsConn, c1)
			// proxy数据转发到client
			go func(c1, c2 net.Conn) {
				defer c2.Close()
				io.Copy(c1, c2)
			}(c1, tlsConn)
		}(conn)
	}
}
