package rudp

import (
	"encoding/binary"
	"sync"
	"time"
)

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
	msgDataC           // c->s，数据
	msgDataS           // s->c，数据
	msgAckC            // c->s，收到数据确认
	msgAckS            // s->c，收到数据确认
	msgCloseC          // c->s，关闭
	msgCloseS          // s->c，关闭
	msgPing            // c->s
	msgPong            // s->c
	msgInvalidC        // c->s
	msgInvalidS        // s->c
)

var msgData = []byte{msgDataC, msgDataS}
var msgAck = []byte{msgAckC, msgAckS}
var msgClose = []byte{msgCloseC, msgCloseS}

const (
	ipV4Header = 20                              // IPv4数据包头大小
	ipV6Header = 40                              // IPv6数据包头大小
	udpHeader  = 8                               // UDP数据包头大小
	minMTU     = 576                             // 链路最小的MTU
	maxMTU     = 1500                            // 链路最大的MTU
	minMSS     = minMTU - ipV6Header - udpHeader // 应用层最小数据包
	maxMSS     = maxMTU - ipV4Header - udpHeader // 应用层最大数据包
	msgVersion = 1                               // 消息协议的版本
	msgBuff    = maxMSS                          // 消息缓存的最大容量
	maxSN      = 0xffffffff - 1                  // 最大的序号，32位即将溢出
)

var _dataPool sync.Pool

func init() {
	_dataPool.New = func() interface{} {
		return new(udpData)
	}
}

// msgType
const msgType = 0

// msgDial
const (
	msgDialVersion     = 1                      // 客户端版本号
	msgDialToken       = msgDialVersion + 4     // 客户端的随机token
	msgDialLocalIP     = msgDialToken + 4       // 客户端的监听ip
	msgDialLocalPort   = msgDialLocalIP + 16    // 客户端的监听端口
	msgDialReadBuffer  = msgDialLocalPort + 2   // 客户端的接收缓存队列长度，窗口控制初始参考值
	msgDialWriteBuffer = msgDialReadBuffer + 4  // 客户端的发送缓存队列长度，窗口控制初始参考值
	msgDialTimeout     = msgDialWriteBuffer + 4 // 客户端的超时
	msgDialLength      = msgDialTimeout + 8     // 消息大小
)

func (this *RUDP) handleMsgDial(msg *udpData) {
	// 没有开启服务
	if this.server.accepted == nil {
		return
	}
	// 检查消息大小和版本
	if msg.n != msgDialLength || msg.b[msgDialVersion] != msgVersion {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgDialToken:])
	// accepting Conn
	var key dialKey
	key.Init(msg.a, token)
	this.server.Lock()
	conn, ok := this.server.accepting[key]
	// 存在，这里不处理
	if ok {
		this.server.Unlock()
		return
	}
	// 不存在，新的连接
	conn = this.newAcceptConn(token, msg.a)
	// 创建失败
	if conn == nil {
		this.server.Unlock()
		// 发送refuse消息
		msg.b[msgType] = msgRefuse
		binary.BigEndian.PutUint32(msg.b[msgRefuseToken:], token)
		this.WriteTo(msg.b[:msgRefuseLength], msg.a)
		// 返回
		return
	}
	// 添加到accepting，connected列表
	this.server.accepting[key] = conn
	this.server.connected[conn.sToken] = conn
	this.server.Unlock()
	// 启动服务端Conn写协程
	go this.acceptConnRoutine(conn, time.Duration(binary.BigEndian.Uint64(msg.b[msgDialTimeout:])))
}

// msgAccept
const (
	msgAcceptVersion     = 1                        // 服务端版本号
	msgAcceptCToken      = msgAcceptVersion + 4     // 客户端发过来的token
	msgAcceptSToken      = msgAcceptCToken + 4      // 服务端的随机token，连接会话token
	msgAcceptClientIP    = msgAcceptSToken + 4      // 客户端的公网ip
	msgAcceptClientPort  = msgAcceptClientIP + 16   // 客户端的公网ip
	msgAcceptReadBuffer  = msgAcceptClientPort + 2  // 服务端的接收缓存队列长度，窗口控制初始参考值
	msgAcceptWriteBuffer = msgAcceptReadBuffer + 4  // 服务端的发送缓存队列长度，窗口控制初始参考值
	msgAcceptLength      = msgAcceptWriteBuffer + 4 // 消息大小
)

func (this *RUDP) handleMsgAccept(msg *udpData) {
	// 检查消息大小和版本
	if msg.n != msgAcceptLength || msg.b[msgAcceptVersion] != msgVersion {
		return
	}
	cToken := binary.BigEndian.Uint32(msg.b[msgAcceptCToken:])
	sToken := binary.BigEndian.Uint32(msg.b[msgAcceptSToken:])
	// dialing Conn
	this.client.RLock()
	conn, ok := this.client.dialing[cToken]
	this.client.RUnlock()
	// 不存在
	if !ok {
		// 发送invalid消息，通知对方
		this.writeMsgInvalid(msg, msgInvalidC, sToken)
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateDial:
		conn.sToken = sToken
		conn.state = connStateConnect
		conn.lock.Unlock()
		// 添加到已连接列表
		this.client.Lock()
		this.client.connected[conn.sToken] = conn
		this.client.Unlock()
		// 启动客户端Conn写协程
		go this.connWriteRoutine(conn)
		// 通知连接
		close(conn.connectSignal)
		// 发送connect消息
		conn.connectMsg(msg)
		this.writeToConn(msg, conn)
	case connStateConnect:
		conn.lock.Unlock()
		// 发送connect消息
		conn.connectMsg(msg)
		this.writeToConn(msg, conn)
	default:
		// 其他状态不处理
		conn.lock.Unlock()
	}
}

// msgRefuse
const (
	msgRefuseVersion = 1                    // 服务端版本号
	msgRefuseToken   = msgRefuseVersion + 4 // 客户端发过来的token
	msgRefuseLength  = msgRefuseToken + 4   // 消息大小
)

func (this *RUDP) handleMsgRefuse(msg *udpData) {
	// 检查消息大小和版本
	if msg.n != msgRefuseLength || msg.b[msgRefuseVersion] != msgVersion {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgRefuseToken:])
	// dialing Conn
	this.client.RLock()
	conn, ok := this.client.dialing[token]
	this.client.RUnlock()
	// 不存在不处理
	if !ok {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateDial:
		// 关闭
		conn.state = connStateClose
		conn.lock.Unlock()
		// 通知
		close(conn.connectSignal)
	default:
		// 其他状态不处理
		// connStateAccept: 客户端不可能
		// connStateClose: 正在关闭
		// connStateConnect: 已经连接
		conn.lock.Unlock()
	}
}

// msgConnect
const (
	msgConnectToken  = 1                   // 连接token
	msgConnectLength = msgConnectToken + 4 // 消息大小
)

func (this *RUDP) handleMsgConnect(msg *udpData) {
	// 没有开启服务
	if this.server.accepted == nil {
		return
	}
	// 检查消息大小
	if msg.n != msgConnectLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgConnectToken:])
	// connected Conn
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	// 不存在
	if !ok {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateAccept:
		// 第一次收到msgConnect
		conn.state = connStateConnect
		conn.lock.Unlock()
		// 通知
		close(conn.connectSignal)
	default:
		// 其他不处理
		// connStateConnect，重复消息
		// connStateDial，服务端不可能
		// connStateClose，关闭了
		conn.lock.Unlock()
	}
}

// msgData
const (
	msgDataToken   = 1                // 连接会话token
	msgDataSN      = msgDataToken + 4 // 数据包序号
	msgDataPayload = msgDataSN + 4    // 数据
)

func (this *RUDP) handleMsgDataC(msg *udpData) {
	// 没有开启服务
	if this.server.accepted == nil {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgDataToken:])
	// server connected Conn
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	// 不存在
	if !ok {
		this.writeMsgInvalid(msg, msgInvalidS, token)
		return
	}
	// 添加数据
	if conn.rBuf.Add(msg, conn.cs) {
		this.writeToConn(msg, conn)
	}
}

func (this *RUDP) handleMsgDataS(msg *udpData) {
	token := binary.BigEndian.Uint32(msg.b[msgDataToken:])
	// client connected Conn
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	// 不存在
	if !ok {
		this.writeMsgInvalid(msg, msgInvalidC, token)
		return
	}
	// 添加数据
	if conn.rBuf.Add(msg, conn.cs) {
		this.writeToConn(msg, conn)
	}
}

// msgAck
const (
	msgAckToken  = 1                // 连接会话token
	msgAckSN     = msgAckToken + 4  // 确认的连续数据包的最大序号
	msgAckMaxSN  = msgAckSN + 4     // 确认的连续数据包的最大序号
	msgAckBuffer = msgAckMaxSN + 4  // 剩余的接受缓存容量长度
	msgAckId     = msgAckBuffer + 4 // ack的序号，用于判断，同一个sn，
	msgAckLength = msgAckId + 8     // 数据大小
)

func (this *RUDP) handleMsgAckC(msg *udpData) {
	// 没有开启服务
	if this.server.accepted == nil {
		return
	}
	if msg.n != msgAckLength {
		return
	}
	this.client.RLock()
	conn, ok := this.client.connected[binary.BigEndian.Uint32(msg.b[msgAckToken:])]
	this.client.RUnlock()
	if !ok {
		_dataPool.Put(msg)
		return
	}
	conn.wBuf.Remove(msg)
}

func (this *RUDP) handleMsgAckS(msg *udpData) {
	if msg.n != msgAckLength {
		return
	}
	this.server.RLock()
	conn, ok := this.server.connected[binary.BigEndian.Uint32(msg.b[msgAckToken:])]
	this.server.RUnlock()
	if !ok {
		_dataPool.Put(msg)
		return
	}
	conn.wBuf.Remove(msg)
}

// msgClose
const (
	msgCloseToken  = 1
	msgCloseLength = msgCloseToken + 4
)

func (this *RUDP) handleMsgCloseC(msg *udpData) {
	// 没有开启服务
	if this.server.accepted == nil {
		return
	}
	if msg.n != msgCloseLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgCloseToken:])
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	if ok {
		conn.Close()
		conn.closeMsg(msg)
		this.writeToConn(msg, conn)
	} else {
		this.writeMsgInvalid(msg, msgInvalidS, token)
	}
}

func (this *RUDP) handleMsgCloseS(msg *udpData) {
	if msg.n != msgCloseLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgCloseToken:])
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	if ok {
		conn.Close()
		conn.closeMsg(msg)
		this.writeToConn(msg, conn)
	} else {
		this.writeMsgInvalid(msg, msgInvalidC, token)
	}
}

// msgPing
const (
	msgPingToken  = 1
	msgPingId     = msgPingToken + 4
	msgPingLength = msgPingId + 4 // 数据大小
)

// 处理Ping消息
func (this *RUDP) handleMsgPing(msg *udpData) {
	if msg.n != msgPingLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgPingToken:])
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	if !ok {
		return
	}
	conn.pongMsg(msg, binary.BigEndian.Uint32(msg.b[msgPingId:]))
	this.writeToConn(msg, conn)
}

// msgPong
const (
	msgPongToken  = 1
	msgPongId     = msgPongToken + 4  // ping传过来的id
	msgPongSN     = msgPongId + 4     // 当前的sn
	msgPongBuffer = msgPongSN + 4     // 剩余的接受缓存容量长度
	msgPongLength = msgPongBuffer + 4 // 数据大小
)

// 处理Pong消息
func (this *RUDP) handleMsgPong(msg *udpData) {
	if msg.n != msgPongLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgPongToken:])
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	if !ok {
		return
	}
	id := binary.BigEndian.Uint32(msg.b[msgPongSN:])
	conn.lock.Lock()
	if id != conn.pingId {
		conn.lock.Unlock()
		return
	}
	conn.pingId++
	left := binary.BigEndian.Uint32(msg.b[msgPongBuffer:])
	conn.wBuf.canWrite = left
	conn.lock.Unlock()
}

// msgInvalid
const (
	msgInvalidToken  = 1
	msgInvalidLength = msgInvalidToken + 4 // 数据大小
)

// 处理invalid消息
func (this *RUDP) handleMsgInvalidC(msg *udpData) {
	if msg.n != msgInvalidLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgInvalidToken:])
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	if ok {
		conn.Close()
	}
}

// 处理invalid消息
func (this *RUDP) handleMsgInvalidS(msg *udpData) {
	if msg.n != msgInvalidLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgInvalidToken:])
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	if ok {
		conn.Close()
	}
}

// 编码invalid消息
func (this *RUDP) writeMsgInvalid(msg *udpData, _type byte, token uint32) {
	msg.b[msgType] = _type
	binary.BigEndian.PutUint32(msg.b[msgInvalidToken:], token)
	msg.n = msgInvalidLength
}
