# tcp_channel
一个加速tcp传输的程序，tcp的算法在某些网络中很慢，可以使用udp来"加速"  
app->tcp->tcp_channel_client->udp->tcp_channel_server->tcp->proxy_server  
## 使用
- client  
命令: tcp_channel client -listen=127.0.0.1:20000 -server=127.0.0.1:10000  
参数:  
-listen: 本地监听的地址，用户app连接的地址  
-server: tcp_channel服务端监听的地址  
- server  
命令: tcp_channel server -listen=127.0.0.1:10000 -proxy=127.0.0.1:30000    
参数:  
-listen: 本地监听的地址，tcp_channel客户端监听的地址  
-proxy: 代理的server地址  