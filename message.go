package rudp

import (
	"encoding/binary"
	"github.com/qq51529210/log"
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
	maxSN      = 0xffffffff - 1                  // 最大的序号，32位即将溢出
)

var _msgPool sync.Pool

func init() {
	_msgPool.New = func() interface{} {
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
	msgDialMSS         = msgDialLocalPort + 2   // 客户端到服务端链路的检测的mss
	msgDialReadBuffer  = msgDialMSS + 2         // 客户端的接收缓存队列长度，窗口控制初始参考值
	msgDialWriteBuffer = msgDialReadBuffer + 4  // 客户端的发送缓存队列长度，窗口控制初始参考值
	msgDialTimeout     = msgDialWriteBuffer + 4 // 客户端的超时
	msgDialLength      = msgDialTimeout + 8     // 数据大小
)

func (this *RUDP) handleMsgDial(msg *udpData) {
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
		// connStateAccepting: 发送accept消息的协程在处理
		// connStateConnected: 已经连接，说明是重复的过期的消息
		// connStateRefused: 服务端不可能，逻辑bug
		// connStateClose: 正在关闭，说明是重复的过期的消息
		this.server.Unlock()
		return
	}
	// 新的连接
	conn = this.newAcceptConn(token, msg.a)
	if conn == nil {
		this.server.Unlock()
		// 发送msgRefuse消息，然后返回
		msg.b[msgType] = msgRefuse
		binary.BigEndian.PutUint32(msg.b[msgRefuseToken:], token)
		// 这里不需要处理错误，如果net.UDPConn出错，readUDPDataRoutine就会退出
		this.writeMessageToConn(msg.b[:msgRefuseLength], msg.a, conn)
		return
	}
	// 添加到列表
	this.server.connected[conn.sToken] = conn
	this.server.accepting[key] = conn
	this.server.Unlock()
	// 启动写协程
	conn.waitQuit.Add(1)
	go this.acceptConnRoutine(conn, token, time.Duration(binary.BigEndian.Uint64(msg.b[msgDialTimeout:])))
}

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

func (this *RUDP) handleMsgAccept(msg *udpData) {
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
			conn.sToken = stoken
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

// msgRefuse
const (
	msgRefuseToken   = 1                    // 客户端发过来的token
	msgRefuseVersion = msgRefuseToken + 4   // 服务端版本号
	msgRefuseLength  = msgRefuseVersion + 4 // 数据大小
)

func (this *RUDP) handleMsgRefuse(msg *udpData) {
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

// msgConnect
const (
	msgConnectToken  = 1                   // 连接会话token
	msgConnectLength = msgConnectToken + 4 // 数据大小
)

func (this *RUDP) handleMsgConnect(msg *udpData) {
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
			conn.Unlock()
			// 通知
			close(conn.connectSignal)
		case connStateConnect:
			// 重复消息，不处理
			conn.Unlock()
		case connStateDial:
			// 服务端不可能有这个状态
			conn.Unlock()
			log.Print(this.logger, log.LevelError, 0, log.FileLineFullPath, "server connect bug")
		default:
			// 其他不处理
			conn.Unlock()
		}
	}
}

// msgData
const (
	msgDataToken   = 1                // 连接会话token
	msgDataSN      = msgDataToken + 4 // 数据包序号
	msgDataPayload = msgDataSN + 4    // 数据
)

func (this *RUDP) handleMsgDataC(msg *udpData) {
	// token
	token := binary.BigEndian.Uint32(msg.b[msgDataToken:])
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	if !ok {
		// 没有
		this.writeMsgInvalid(msg, token)
		_msgPool.Put(msg)
		return
	}
	this.handleMsgData(conn, msg, csServer)
}

func (this *RUDP) handleMsgDataS(msg *udpData) {
	// token
	token := binary.BigEndian.Uint32(msg.b[msgDataToken:])
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	if !ok {
		// 没有
		this.writeMsgInvalid(msg, token)
		_msgPool.Put(msg)
		return
	}
	this.handleMsgData(conn, msg, csClient)
}

func (this *RUDP) handleMsgData(conn *Conn, msg *udpData, cs cs) {
	sn := binary.BigEndian.Uint32(msg.b[msgDataSN:])
	rb := conn.readBuffer
	// 检查sn是否在接收缓存范围
	rb.lock.Lock()
	// 超过了接收缓存的最大序列号
	if sn >= rb.maxSN {
		rb.lock.Unlock()
		return
	}
	// 是新的数据
	if sn >= rb.nextSN {
		conn.readBuffer.Add(sn, msg.b[msgDataPayload:msg.n])
	}
	// 可读
	if sn == rb.nextSN {
		rb.nextSN++
		rb.ackId = 0
		select {
		case rb.enable <- 1:
		default:
		}
	}
	// 剩余的容量
	left := rb.Left()
	ack := rb.ackId
	rb.ackId++
	rb.lock.Unlock()
	// 响应ack
	msg.b[msgType] = msgAck[cs]
	binary.BigEndian.PutUint32(msg.b[msgAckToken:], conn.sToken)
	binary.BigEndian.PutUint32(msg.b[msgAckSN:], sn)
	binary.BigEndian.PutUint32(msg.b[msgAckBuffer:], left)
	binary.BigEndian.PutUint64(msg.b[msgAckId:], ack)
	// 这里不处理错误
	this.WriteTo(msg.b[:msgAckLength], msg.a)
	conn.totalBytes.w += uint64(msgAckLength)
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
	this.client.RLock()
	conn, ok := this.client.connected[binary.BigEndian.Uint32(msg.b[msgAckToken:])]
	this.client.RUnlock()
	if !ok {
		_msgPool.Put(msg)
		return
	}
	this.handleMsgAck(conn, msg)
}

func (this *RUDP) handleMsgAckS(msg *udpData) {
	this.client.RLock()
	conn, ok := this.client.connected[binary.BigEndian.Uint32(msg.b[msgAckToken:])]
	this.client.RUnlock()
	if !ok {
		_msgPool.Put(msg)
		return
	}
	this.handleMsgAck(conn, msg)
}

func (this *RUDP) handleMsgAck(conn *Conn, msg *udpData) {
	// 检查发送队列
	conn.writeBuffer.Lock()
	if conn.writeBuffer.data == nil {
		conn.writeBuffer.Unlock()
		return
	}
	sn := binary.BigEndian.Uint32(msg.b[msgAckSN:])
	max_sn := binary.BigEndian.Uint32(msg.b[msgAckMaxSN:])
	p := conn.writeBuffer.data
	ok := false
	if max_sn >= sn {
		// 从发送队列移除小于max_sn的数据块
		for p != nil {
			if p.sn > sn {
				break
			}
			if p.sn == sn {
				// 计算RTO
				conn.writeBuffer.CalcRTO(p)
			}
			// 移除
			n := p.next
			conn.writeBuffer.putData(p)
			p = n
		}
		conn.writeBuffer.data = p
		conn.writeBuffer.UpdateAck(max_sn, msg)
		ok = true
	} else {
		// 从发送队列移除指定sn的数据块
		if p.sn == sn {
			// 链表头
			conn.writeBuffer.data = p.next
			conn.writeBuffer.UpdateAck(sn, msg)
			conn.writeBuffer.putData(p)
			ok = true
		} else {
			next := p.next
			for next != nil {
				if next.sn > sn {
					break
				}
				if next.sn == sn {
					// 计算RTO
					conn.writeBuffer.CalcRTO(next)
					// 移除
					p.next = next.next
					conn.writeBuffer.putData(next)
					conn.writeBuffer.UpdateAck(sn, msg)
					ok = true
					break
				}
				next = p.next
			}
		}
	}
	conn.writeBuffer.Unlock()
	// 是否可写入新数据
	if ok {
		conn.writeBuffer.Signal()
	}
}

// msgClose
const (
	msgCloseToken  = 1
	msgCloseLength = msgCloseToken + 4
)

func (this *RUDP) handleMsgCloseC(msg *udpData) {
	token := binary.BigEndian.Uint32(msg.b[msgCloseToken:])
	this.client.RLock()
	conn, ok := this.server.connected[token]
	if ok {
		var key dialKey
		key.Init(conn.rAddr, conn.cToken)
		delete(this.server.accepting, key)
		delete(this.server.connected, token)
		conn.Close()
	}
	this.server.Unlock()
	this.writeMsgClose(msg, token, csServer)
}

func (this *RUDP) handleMsgCloseS(msg *udpData) {
	token := binary.BigEndian.Uint32(msg.b[msgCloseToken:])
	this.client.Lock()
	conn, ok := this.client.connected[token]
	if ok {
		delete(this.client.dialing, conn.cToken)
		delete(this.client.connected, token)
		conn.Close()
	}
	this.client.Unlock()
	this.writeMsgClose(msg, token, csClient)
}

// msgPingPong
const (
	msgPingPongToken  = 1
	msgPingPongBuffer = msgPingPongToken + 4  // 剩余的接受缓存容量长度
	msgPingPongLength = msgPingPongBuffer + 4 // 数据大小
)

// 处理消息msgPing
func (this *RUDP) handleMsgPing(msg *udpData) {

}

// 处理消息msgPong
func (this *RUDP) handleMsgPong(msg *udpData) {

}

// 发送消息msgConnect
func (this *RUDP) writeMsgConnect(msg *udpData, token uint32) {
	binary.BigEndian.PutUint32(msg.b[msgConnectToken:], token)
	this.conn.WriteToUDP(msg.b[:msgConnectLength], msg.a)
}

// 发送消息msgClose
func (this *RUDP) writeMsgClose(msg *udpData, token uint32, cs cs) {
	msg.b[msgType] = msgClose[cs]
	binary.BigEndian.PutUint32(msg.b[msgCloseToken:], token)
	this.conn.WriteToUDP(msg.b[:msgCloseLength], msg.a)
}

// msgInvalid
const (
	msgInvalidToken  = 1
	msgInvalidLength = msgInvalidToken + 4
)

func (this *RUDP) handleMsgInvalid(msg *udpData) {

}

func (this *RUDP) writeMsgInvalid(msg *udpData, token uint32) {
	msg.b[msgType] = msgInvalid
	binary.BigEndian.PutUint32(msg.b[msgInvalidToken:], token)
	this.conn.WriteToUDP(msg.b[:msgInvalidLength], msg.a)
}
