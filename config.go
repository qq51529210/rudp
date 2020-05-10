package rudp

type Config struct {
	Listen          string `json:"listen"`            // udp监听地址，不能为空
	UDPDataQueue    int    `json:"udp_data_queue"`    // udp原始数据缓存队列，默认是1024
	UDPDataRoutine  int    `json:"udp_data_routine"`  // udp原始数据处理协程个数，默认是runtime.NumCPU()
	ConnReadBuffer  int    `json:"conn_read_buffer"`  // conn的读缓存大小，用于窗口控制，默认是1024*4
	ConnWriteBuffer int    `json:"conn_write_buffer"` // conn的写缓存大小，用于窗口控制，默认是1024*4
	AcceptQueue     int    `json:"accept_queue"`      // 作为服务端时，接受连接的队列。为0则不作为server，Accept()函数返回错误
	AcceptRTO       int    `json:"accept_rto"`        // 作为服务端时，accept消息超时重发，单位ms，默认100ms
	DialRTO         int    `json:"dial_rto"`          // 作为客户端时，dial消息超时重发，单位ms，默认100ms
}
