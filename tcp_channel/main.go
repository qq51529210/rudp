package main

import (
	"flag"
	"fmt"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage: tcp_channel COMMAND

Commands:
	client		run as client proxy
	server		run as server proxy

Run 'tcp_channel COMMAND --help' for more information on a command
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	// 命令行
	switch flag.Arg(0) {
	case "client":
		runClient()
	case "server":
		runServer()
	default:
		flag.Usage()
	}
}
