package rudp

// 消息类型定义
// 为什么Data/Ack/Close要区分cs
// RUDP是一个双向的（可作为c也可作为s），一对多的Conn
// 创建连接时有可能token相同（已区分成两个map）
// conn1=(a-token1 dial b-token1) / conn2(b-token2 dial a-token2)
// 双向连接时，假设，a-token1==b-token2 && b-token1==a-token2
// 这样的情况，如果不区分c/s消息，单靠一个token无法定位到Conn1/Conn2
const (
	msgExtend   = iota // 可扩展的消息
	msgDial            // c->s，请求创建连接，握手1
	msgAccept          // s->c，接受连接，握手2
	msgRefuse          // s->c，拒绝连接，握手2
	msgConnect         // c->s，收到接受连接的消息，握手3
	msgPing            // c->s，ping
	msgPong            // s->c，pong
	msgDataC           // c->s，数据
	msgDataS           // s->c，数据
	msgAckC            // c->s，确认数据
	msgAckS            // s->c，确认数据
	msgCloseC          // c->s，关闭
	msgCloseS          // s->c，关闭
	msgInvalidC        // c->s，无效连接
	msgInvalidS        // s->c，无效连接
)

// msgDial字段下标
const (
	msgDialVersion     = 1                      // 版本号
	msgDialToken       = msgDialVersion + 4     // 随机token
	msgDialLocalIP     = msgDialToken + 4       // 本地监听ip
	msgDialLocalPort   = msgDialLocalIP + 16    // 本地监听端口
	msgDialRemoteIP    = msgDialLocalPort + 4   // 对方公网ip
	msgDialRemotePort  = msgDialRemoteIP + 16   // 对方公网端口
	msgDialMSS         = msgDialRemotePort + 2  // udp数据包大小，窗口控制参考值
	msgDialReadBuffer  = msgDialMSS + 2         // 接收缓存大小（字节），窗口控制参考值
	msgDialWriteBuffer = msgDialReadBuffer + 4  // 发送缓存大小（字节），窗口控制参考值
	msgDialTimeout     = msgDialWriteBuffer + 4 // Dial函数的连接超时
	msgDialLength      = msgDialTimeout + 8
)

// msgAccept字段下标
const (
	msgAcceptVersion     = 1                       // 版本号
	msgAcceptCToken      = msgAcceptVersion + 4    // 客户端token
	msgAcceptSToken      = msgAcceptCToken + 4     // 服务端token
	msgAcceptLocalIP     = msgAcceptSToken + 4     // 本地监听ip
	msgAcceptLocalPort   = msgAcceptLocalIP + 16   // 本地监听端口
	msgAcceptRemoteIP    = msgAcceptLocalPort + 4  // 对方公网ip
	msgAcceptRemotePort  = msgAcceptRemoteIP + 16  // 对方公网端口
	msgAcceptMSS         = msgAcceptRemotePort + 2 // udp数据包大小，窗口控制参考值
	msgAcceptReadBuffer  = msgAcceptMSS + 2        // 接收缓存大小（字节），窗口控制参考值
	msgAcceptWriteBuffer = msgAcceptReadBuffer + 4 // 发送缓存大小（字节），窗口控制参考值
	msgAcceptLength      = msgAcceptWriteBuffer + 4
)

// msgRefuse字段下标
const (
	msgRefuseVersion = 1                    // 版本号
	msgRefuseToken   = msgRefuseVersion + 4 // 客户端token
	msgRefuseLength  = msgRefuseToken + 4
)

// msgConnect字段下标
const (
	msgConnectCToken = 1                    // 客户端token
	msgConnectSToken = msgConnectCToken + 4 // 服务端token
	msgConnectLength = msgConnectSToken + 4
)

// msgData字段下标
const (
	msgDataCToken  = 1                 // 客户端token
	msgDataSToken  = msgDataCToken + 4 // 服务端token
	msgDataSN      = msgDataSToken + 4 // 数据包的序号
	msgDataPayload = msgDataSN + 4
)

// msgAck字段下标
const (
	msgAckCToken  = 1                 // 客户端token
	msgAckSToken  = msgAckCToken + 4  // 服务端token
	msgAckSN      = msgAckSToken + 4  // 数据包的序号
	msgAckMaxSN   = msgAckSN + 4      // 连续数据包的序号
	msgAckRemains = msgAckMaxSN + 4   // 剩余的接受缓存容量长度
	msgAckId      = msgAckRemains + 4 // ack的序号，用于判断，同一个sn，
	msgAckLength  = msgAckId + 8
)

// msgClose字段下标
const (
	msgCloseCToken = 1                  // 客户端token
	msgCloseSToken = msgCloseCToken + 4 // 服务端token
	msgCloseState  = msgCloseSToken + 4 // 关闭连接的状态
	msgCloseLength = msgCloseState + 1
)

// msgPing字段下标
const (
	msgPingCToken = 1                 // 客户端token
	msgPingSToken = msgPingCToken + 4 // 服务端token
	msgPingId     = msgPingSToken + 4 // 每一个ping消息的id，递增
	msgPingLength = msgPingId + 4
)

// msgPong字段下标
const (
	msgPongCToken  = 1                 // 客户端token
	msgPongSToken  = msgPongCToken + 4 // 服务端token
	msgPongPingId  = msgPongSToken + 4 // ping传过来的id
	msgPongSN      = msgPongPingId + 4 // 连续数据包的序号
	msgPongRemains = msgPongSN + 4     // 剩余的接受缓存容量长度
	msgPongLength  = msgPongRemains + 4
)

// msgInvalid字段下标
const (
	msgInvalidCToken = 1                    // 客户端token
	msgInvalidSToken = msgInvalidCToken + 4 // 服务端token
	msgInvalidLength = msgInvalidSToken + 4
)

const (
	msgType    = 0      // 消息类型下标
	msgVersion = 1      // 消息的版本，不同的版本可能字段不一样
	msgBuffLen = maxMSS // 消息缓存大小，udpData中使用
)
