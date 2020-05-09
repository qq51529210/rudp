package rudp

import (
	"encoding/binary"
	"net"
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
	msgExtend  = iota // 可扩展的消息
	msgDial           // c->s，请求创建连接，握手1
	msgAccept         // s->c，接受连接，握手2
	msgRefuse         // s->c，拒绝连接，握手2
	msgConnect        // c->s，收到接受连接的消息，握手3
	msgDataC          // c->s，数据
	msgDataS          // s->c，数据
	msgAckC           // c->s，收到数据确认
	msgAckS           // s->c，收到数据确认
	msgCloseC         // c->s，关闭
	msgCloseS         // s->c，关闭
	msgPing           // c->s
	msgPong           // s->c
	msgInvalid        // s<->c，无效的连接
)

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
)

var _msgPool sync.Pool

func init() {
	_msgPool.New = func() interface{} {
		return new(message)
	}
}

type message struct {
	b [msgBuff]byte // 数据缓存
	n int           // 数据大小
	a *net.UDPAddr  // 对方的地址
}

func (this *message) Type() byte {
	return this.b[0]
}

func (this *message) Bytes() []byte {
	return this.b[:this.n]
}

// msgType
const msgType = 0

// msgDial
const (
	msgDialVersion     = 1                      // 客户端版本号
	msgDialToken       = msgDialVersion + 4     // 客户端的随机token
	msgDialLocalIP     = msgDialToken + 4       // 客户端的监听ip
	msgDialLocalPort   = msgDialLocalIP + 16    // 客户端的监听端口
	msgDialMSS         = msgDialLocalPort + 2   // 客户端到服务端链路的检测的mss
	msgDialReadBuffer  = msgDialMSS + 2         // 客户端的接收缓存队列长度，窗口控制初始参考值
	msgDialWriteBuffer = msgDialReadBuffer + 4  // 客户端的发送缓存队列长度，窗口控制初始参考值
	msgDialTimeout     = msgDialWriteBuffer + 4 // 客户端的超时
	msgDialLength      = msgDialTimeout + 8     // 数据大小
)

// msgAccept
const (
	msgAcceptVersion     = 1                        // 服务端版本号
	msgAcceptCToken      = msgAcceptVersion + 4     // 客户端发过来的token
	msgAcceptSToken      = msgAcceptCToken + 4      // 服务端的随机token，连接会话token
	msgAcceptClientIP    = msgAcceptSToken + 4      // 客户端的公网ip
	msgAcceptClientPort  = msgAcceptClientIP + 16   // 客户端的公网ip
	msgAcceptMSS         = msgAcceptClientPort + 2  // 服务端到客户端链路的检测的mss
	msgAcceptReadBuffer  = msgAcceptMSS + 2         // 服务端的接收缓存队列长度，窗口控制初始参考值
	msgAcceptWriteBuffer = msgAcceptReadBuffer + 4  // 服务端的发送缓存队列长度，窗口控制初始参考值
	msgAcceptLength      = msgAcceptWriteBuffer + 4 // 数据大小
)

// msgRefuse
const (
	msgRefuseToken   = 1                    // 客户端发过来的token
	msgRefuseVersion = msgRefuseToken + 4   // 服务端版本号
	msgRefuseLength  = msgRefuseVersion + 4 // 数据大小
)

// msgConnect
const (
	msgConnectToken  = 1                   // 连接会话token
	msgConnectLength = msgConnectToken + 4 // 数据大小
)

// msgData
const (
	msgDataToken   = 1                // 连接会话token
	msgDataSN      = msgDataToken + 4 // 数据包序号
	msgDataPayload = msgDataSN + 4    // 数据
)

// msgAck
const (
	msgAckToken  = 1                // 连接会话token
	msgAckSN     = msgAckToken + 4  // 确认的连续数据包的最大序号
	msgAckBuffer = msgAckSN + 4     // 剩余的接受缓存容量长度
	msgAckLength = msgAckBuffer + 4 // 数据大小
)

// msgPingPong
const (
	msgPingPongToken  = 1
	msgPingPongBuffer = msgPingPongToken + 4  // 剩余的接受缓存容量长度
	msgPingPongLength = msgPingPongBuffer + 4 // 数据大小
)

// msgClose
const (
	msgCloseToken  = 1
	msgCloseLength = msgCloseToken + 4
)

// msgInvalid
const (
	msgInvalidToken  = 1
	msgInvalidLength = msgInvalidToken + 4
)

// 处理消息msgDial
func (this *RUDP) handleMsgDial(msg *message) {
	// 检查消息大小和版本
	if msg.n != msgDialLength || msg.b[msgDialVersion] != msgVersion {
		return
	}
	// token
	token := binary.BigEndian.Uint32(msg.b[msgDialToken:])
	// 检查token是否存在server.accepting
	var key dialKey
	key.Init(msg.a, token)
	this.server.Lock()
	conn, ok := this.server.accepting[key]
	if ok {
		// 存在，这里不处理
		// connStateAccepting: 发送msgAccept协程在处理
		// connStateConnected: 已经连接，说明是重复的过期的消息
		// connStateRefused: 服务端不可能，逻辑bug
		// connStateClose: 正在关闭，说明是重复的过期的消息
		this.server.Unlock()
		return
	}
	// 不存在，第一次收到消息
	conn = this.newConn(connStateAccept, msg.a)
	// 产生服务端token
	conn.token = this.server.token
	this.server.token++
	tkn := conn.token
	for {
		// 是否存在
		_, ok = this.server.connected[conn.token]
		if !ok {
			break
		}
		conn.token = this.server.token
		this.server.token++
		// 连接耗尽，2^32，理论上现有计算机不可能
		if conn.token == tkn {
			this.server.Unlock()
			// 发送msgRefuse消息，然后返回
			this.writeMsgRefuse(msg, token)
			return
		}
	}
	// 添加到列表
	this.server.connected[conn.token] = conn
	this.server.accepting[key] = conn
	this.server.Unlock()
	// 启动写协程
	conn.waitQuit.Add(1)
	go this.acceptConnRoutine(conn, token, time.Duration(binary.BigEndian.Uint64(msg.b[msgDialTimeout:])))
}

// 处理消息msgAccept
func (this *RUDP) handleMsgAccept(msg *message) {
	// 检查消息大小和版本
	if msg.n != msgAcceptLength || msg.b[msgAcceptVersion] != msgVersion {
		return
	}
	// token对
	ctoken := binary.BigEndian.Uint32(msg.b[msgAcceptCToken:])
	stoken := binary.BigEndian.Uint32(msg.b[msgAcceptSToken:])
	// 检查ctoken是否存在client.dialing
	this.client.RLock()
	conn, ok := this.client.dialing[ctoken]
	this.client.RUnlock()
	if ok {
		conn.Lock()
		switch conn.connState {
		case connStateDial:
			conn.token = stoken
			conn.connState = connStateConnect
			conn.Unlock()
			// 通知
			close(conn.connectSignal)
			// 发送msgConnect
			this.writeMsgConnect(msg, stoken)
		case connStateConnect:
			// 重复收到msgAccept
			conn.Unlock()
			// 发送msgConnect
			this.writeMsgConnect(msg, stoken)
		default:
			// 其他不处理
			conn.Unlock()
		}
	}
}

// 处理消息msgRefuse
func (this *RUDP) handleMsgRefuse(msg *message) {
	// 检查消息大小和版本
	if msg.n != msgRefuseLength || msg.b[msgRefuseVersion] != msgVersion {
		return
	}
	// 检查token是否存在client.dialing
	token := binary.BigEndian.Uint32(msg.b[msgRefuseToken:])
	this.client.RLock()
	conn, ok := this.client.dialing[token]
	this.client.RUnlock()
	// 存在，并且仍然是connStateDial
	if ok {
		conn.Lock()
		if conn.connState == connStateDial {
			conn.connState = connStateClose
			close(conn.connectSignal)
		}
		conn.Unlock()
	}
}

// 处理消息msgConnect
func (this *RUDP) handleMsgConnect(msg *message) {
	// 检查消息大小和版本
	if msg.n != msgConnectLength {
		return
	}
	// 检查token是否存在client.dialing
	token := binary.BigEndian.Uint32(msg.b[msgConnectToken:])
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	if !ok {
		conn.Lock()
		switch conn.connState {
		case connStateAccept:
			// 第一次收到msgConnect
			conn.connState = connStateConnect
			close(conn.connectSignal)
		case connStateConnect:
			// 重复消息，不处理
		case connStateDial:
			// 服务端不可能有这个状态
			panic("connect bug")
		default:
			// 其他不处理
		}
		conn.Unlock()
	}
}

// 处理消息msgDataC
func (this *RUDP) handleMsgDataC(msg *message) {
	// token
	token := binary.BigEndian.Uint32(msg.b[msgDataPayload:])
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	if !ok {
		// 没有
		this.writeMsgInvalid(msg, token)
		_msgPool.Put(msg)
		return
	}
	this.handleMsgData(conn, msg)
}

// 处理消息msgDataS
func (this *RUDP) handleMsgDataS(msg *message) {
	// token
	token := binary.BigEndian.Uint32(msg.b[msgDataPayload:])
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	if !ok {
		// 没有
		this.writeMsgInvalid(msg, token)
		_msgPool.Put(msg)
		return
	}
	this.handleMsgData(conn, msg)
}

// 处理消息msgData
func (this *RUDP) handleMsgData(conn *Conn, msg *message) {
	// sn
	sn := binary.BigEndian.Uint32(msg.b[msgDataSN:])
	conn.readBuffer.Lock()
	// 是否超过了接收缓存的最大序列号
	if sn >= conn.readBuffer.maxSN {
		conn.readBuffer.Unlock()
		_msgPool.Put(msg)
		return
	}
	var free int
	// 是否小于想接收的序列号，是重复的数据包
	if sn < conn.readBuffer.sn {
		free = conn.readBuffer.data.Free()
	} else {
		// 检查队列
		if conn.readBuffer.data.first == nil {
			d := _dataPool.Get().(*data)
			d.sn = sn
			d.buf = d.buf[:0]
			d.buf = append(d.buf, msg.b[msgDataPayload:msg.n]...)
			conn.readBuffer.data.first = d
		} else {
			p := conn.readBuffer.data.first
			for {
				// 新的数据
				if p == nil {
					break
				}
			}
		}
		free = conn.readBuffer.data.Free()
	}
	conn.readBuffer.Unlock()
	// 响应ack
	this.writeMsgAck(msg, conn.token, uint32(free))
}

// 处理消息msgAckC
func (this *RUDP) handleMsgAckC(msg *message) {

}

// 处理消息msgAckS
func (this *RUDP) handleMsgAckS(msg *message) {

}

// 处理消息msgCloseC
func (this *RUDP) handleMsgCloseC(msg *message) {

}

// 处理消息msgCloseS
func (this *RUDP) handleMsgCloseS(msg *message) {

}

// 处理消息msgPing
func (this *RUDP) handleMsgPing(msg *message) {

}

// 处理消息msgPong
func (this *RUDP) handleMsgPong(msg *message) {

}

// 发送消息msgRefuse
func (this *RUDP) writeMsgRefuse(msg *message, token uint32) {
	msg.b[msgType] = msgRefuse
	binary.BigEndian.PutUint32(msg.b[msgRefuseToken:], token)
	this.conn.WriteToUDP(msg.b[:msgRefuseLength], msg.a)
}

// 发送消息msgConnect
func (this *RUDP) writeMsgConnect(msg *message, token uint32) {
	binary.BigEndian.PutUint32(msg.b[msgConnectToken:], token)
	this.conn.WriteToUDP(msg.b[:msgConnectLength], msg.a)
}

// 发送消息msgAck
func (this *RUDP) writeMsgAck(msg *message, token, length uint32) {
	msg.b[msgType] = msgInvalid
	binary.BigEndian.PutUint32(msg.b[msgInvalidToken:], token)
	this.conn.WriteToUDP(msg.b[:msgInvalidLength], msg.a)
}

// 发送消息msgClose
func (this *RUDP) writeMsgClose(msg *message, token uint32, _type byte) {
	msg.b[msgType] = _type
	binary.BigEndian.PutUint32(msg.b[msgCloseToken:], token)
	this.conn.WriteToUDP(msg.b[:msgCloseLength], msg.a)
}

// 发送消息msgInvalid
func (this *RUDP) writeMsgInvalid(msg *message, token uint32) {
	msg.b[msgType] = msgInvalid
	binary.BigEndian.PutUint32(msg.b[msgInvalidToken:], token)
	this.conn.WriteToUDP(msg.b[:msgInvalidLength], msg.a)
}
