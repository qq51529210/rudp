package rudp

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

// V4地址转V6整数时用
const v4InV6Prefix = uint64(0xff)<<40 | uint64(0xff)<<32

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

func newServer(cfg *Config) *server {
	p := new(server)
	if cfg.AcceptQueue > 0 {
		p.accepted = make(chan *Conn, cfg.AcceptQueue)
	}
	p.connected = make(map[uint32]*Conn)
	p.accepting = make(map[dialKey]*Conn)
	p.token = _rand.Uint32()
	p.acceptRTO = time.Duration(defaultInt(cfg.AcceptRTO, 100)) * time.Millisecond
	return p
}

type server struct {
	sync.RWMutex                   // 锁
	accepted     chan *Conn        // 已经建立的连接，等待应用层处理
	connected    map[uint32]*Conn  // 已经建立的连接
	accepting    map[dialKey]*Conn // 正在建立的连接
	token        uint32            // 随机token，用于确认连接会话
	acceptRTO    time.Duration     // accept消息超时重发
}

// 设置msgAccept的超时重发
func (this *RUDP) SetAcceptRTO(rto time.Duration) {
	if rto > 0 {
		this.server.acceptRTO = rto
	}
}

// net.Listener接口
func (this *RUDP) Accept() (net.Conn, error) {
	// 不作为服务端
	if this.server.accepted == nil {
		return nil, this.netOpError("listen", opErr("rudp is no a server"))
	}
	select {
	case conn, ok := <-this.server.accepted:
		// 等待新的连接
		if ok {
			return conn, nil
		}
	case <-this.closeSignal:
		// rudp关闭信号
	}
	return nil, this.netOpError("listen", closeErr("rudp"))
}

// net.Listener接口
func (this *RUDP) Addr() net.Addr {
	return this.conn.LocalAddr()
}

// 释放client相关的资源
func (this *RUDP) closeServer() {
	// 释放server相关的资源
	if this.server.accepted != nil {
		close(this.server.accepted)
	}
	for _, c := range this.server.connected {
		c.Close()
	}
}

// 服务端Conn写协程
func (this *RUDP) acceptConnRoutine(conn *Conn, timeout time.Duration) {
	this.wait.Add(1)
	// 开始时间
	start_time := time.Now()
	// 初始化accept消息
	msg := _dataPool.Get().(*udpData)
	conn.acceptMsg(msg)
	// 超时计时器
	timer := time.NewTimer(0)
	defer func() {
		timer.Stop()
		this.closeConn(conn)
		this.wait.Done()
	}()
	// 发送accept消息循环
AccepLoop:
	for {
		select {
		case now := <-timer.C:
			// 超时
			if now.Sub(start_time) >= timeout {
				// 关闭
				conn.Close()
				return
			}
			// 重发
			this.writeToConn(msg, conn)
			// 重置计时器
			timer.Reset(this.server.acceptRTO)
		case <-this.closeSignal:
			// rudp关闭信号
			return
		case <-conn.connectSignal:
			// Conn连接通知
			break AccepLoop
		case <-conn.closeSignal:
			// Conn关闭信号
			return
		}
	}
	for {
		select {
		case <-this.closeSignal:
			// rudp关闭信号
			return
		case <-conn.closeSignal:
			// Conn关闭信号
			return
		case now := <-timer.C:
			// 超时重传检查
			this.checkWriteBuffer(conn, now)
			// 重置计时器
			timer.Reset(this.server.acceptRTO)
		}
	}
}

// 创建一个新的服务端Conn，拆分handleMsgDial()代码
func (this *RUDP) newAcceptConn(cToken uint32, cAddr *net.UDPAddr) *Conn {
	// 不存在，第一次收到消息
	conn := this.newConn(connStateAccept, cAddr, DetectMSS(cAddr), csS)
	// 产生服务端token
	conn.cToken = cToken
	conn.sToken = this.server.token
	this.server.token++
	tkn := conn.sToken
	for {
		// 是否存在
		_, ok := this.server.connected[conn.sToken]
		if !ok {
			break
		}
		conn.sToken = this.server.token
		this.server.token++
		// 连接耗尽，2^32，理论上现有计算机不可能
		if conn.sToken == tkn {
			return nil
		}
	}
	return conn
}
