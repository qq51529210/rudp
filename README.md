# rudp
一个基于udp协议的可靠性传输库，主要的结构有
1. RUDP，实现了net.Listener接口，和一个Dial函数
2. Conn，实现了net.Conn接口  
## 用法 
代码在[rudp_test.go](./rudp_test.go)中
## tcp_channel
使用rudp实现的一个tcp转udp的代理程序 
