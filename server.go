package rudp

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

const (
	defaultAcceptBacklog = 64
	defaultAcceptRTO     = time.Millisecond * 100 // 发送msgAccept的时间间隔
)

const v4InV6Prefix = uint64(0xff)<<40 | uint64(0xff)<<32 // V4地址转V6整数时用

// 表示正在创建连接的键，因为不同的client产生的token可能相同，所以加上地址key
type dialKey struct {
	ip1   uint64 // IPV6转整数前64位
	ip2   uint64 // IPV6转整数后64位
	port  uint16 // 端口
	token uint32 // 客户端32位随机码
}

func (this *dialKey) Init(addr *net.UDPAddr, token uint32) {
	switch len(addr.IP) {
	case net.IPv4len:
		this.ip1 = 0
		this.ip2 = v4InV6Prefix | uint64(addr.IP[0])<<24 |
			uint64(addr.IP[1])<<16 | uint64(addr.IP[2])<<8 | uint64(addr.IP[3])
	case net.IPv6len:
		this.ip1 = binary.BigEndian.Uint64(addr.IP[0:])
		this.ip2 = binary.BigEndian.Uint64(addr.IP[8:])
	}
	this.port = uint16(addr.Port)
	this.token = token
}

func newServer() *server {
	p := new(server)
	p.accepted = make(chan *Conn, defaultAcceptBacklog)
	p.connected = make(map[uint32]*Conn)
	p.accepting = make(map[dialKey]*Conn)
	p.token = _rand.Uint32()
	p.acceptRTO = defaultAcceptRTO
	return p
}

type server struct {
	sync.RWMutex                   // 锁
	accepted     chan *Conn        // 已经建立连接，等待应用层处理
	connected    map[uint32]*Conn  // 已经建立的连接
	accepting    map[dialKey]*Conn // 正在建立的连接
	token        uint32            // 随机token
	acceptRTO    time.Duration     // 发送msgAccept的时间间隔
}

// 设置msgAccept的超时重发
func (this *RUDP) SetAcceptRTO(rto time.Duration) {
	if rto > 0 {
		this.server.acceptRTO = rto
	}
}

// net.Listener接口
func (this *RUDP) Accept() (net.Conn, error) {
	select {
	case Conn, ok := <-this.server.accepted: // 等待新的连接
		if ok {
			return Conn, nil
		}
	case <-this.closeSignal: // 退出信号
	}
	return nil, this.opError("listen", nil, errClosed)
}

// net.Listener接口
func (this *RUDP) Addr() net.Addr {
	return this.conn.LocalAddr()
}

// 释放server相关的资源
func (this *RUDP) closeServer() {
	close(this.server.accepted)
	for _, c := range this.server.connected {
		c.Close()
	}
}

// 服务端Conn写协程
func (this *RUDP) acceptConnRoutine(conn *Conn, cToken uint32, cto time.Duration) {
	defer conn.waitQuit.Done()
	// 初始化msgAccept
	var msg message
	this.encodeMsgAccept(conn, cToken, &msg)
	// 发送msgAccept
	this.writeMsgAcceptLoop(conn, &msg, cto)
	// 检查发送队列
	timer := time.NewTimer(conn.rto)
	var n int
	var err error
	for {
		select {
		case <-this.closeSignal:
			// rudp被关闭
			return
		case <-conn.closeSignal:
			// conn被关闭
			return
		case now := <-timer.C:
			// 重传超时
			n, err = conn.checkWriteBuffer(this.conn, now)
			if err != nil {
				return
			}
			conn.rwBytes.w += uint64(n)
			this.rwBytes.w += uint64(n)
		}
	}
}

// 发送msgAccept循环
func (this *RUDP) writeMsgAcceptLoop(conn *Conn, msg *message, cto time.Duration) {
	ticker := time.NewTicker(this.server.acceptRTO)
	defer ticker.Stop()
	// 发送msgAccept
	var n int
	var err error
	start_time := time.Now()
	for {
		select {
		case now := <-ticker.C:
			// 是否超时
			if now.Sub(start_time) >= cto {
				// 关闭
				conn.Close()
				return
			}
			// 发送消息间隔
			n, err = this.conn.WriteToUDP(msg.b[:msgAcceptLength], conn.rAddr)
			if err != nil {
				return
			}
			// 统计
			conn.rwBytes.w += uint64(n)
			this.rwBytes.w += uint64(n)
		case <-this.closeSignal:
			// rudp被关闭
			return
		case <-conn.connectSignal:
			// 连接通知
			return
		case <-conn.closeSignal:
			// 连接关闭
			return
		}
	}
}

// 编码msgAccept，拆分acceptConnRoutine()代码
func (this *RUDP) encodeMsgAccept(conn *Conn, cToken uint32, msg *message) {
	msg.b[msgType] = msgAccept
	binary.BigEndian.PutUint32(msg.b[msgAcceptCToken:], cToken)
	binary.BigEndian.PutUint32(msg.b[msgAcceptSToken:], conn.token)
	copy(msg.b[msgAcceptClientIP:], conn.rAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.b[msgAcceptClientPort:], uint16(conn.rAddr.Port))
	binary.BigEndian.PutUint16(msg.b[msgAcceptMSS:], conn.mss)
	binary.BigEndian.PutUint32(msg.b[msgAcceptReadBuffer:], uint32(conn.rBuf.length))
	binary.BigEndian.PutUint32(msg.b[msgAcceptWriteBuffer:], uint32(conn.wBuf.length))
}
