package rudp

import (
	"container/list"
	"net"
	"sync"
	"time"
)

//type connState int
//
//const (
//	connStateInvalid   connState = iota // 无效状态，即将移除
//	connStateDialing                    // 客户端正在拔号连接，发送dial消息
//	connStateAccepting                  // 服务端收到dial消息，建立连接，发送accept消息，等待客户端connect消息
//	connStateRefused                    // 客户端收到refuse消息，连接被服务端拒绝
//	connStateConnected                  // 客户端收到accept消息，或服务端收到connect消息，连接双向确认。
//	connStateClosing                    // 应用层调用rut.conn.Close()函数，发送close消息，等待对方确认。
//	connStateClosed                     // 收到对方的close消息，等待对方确认。
//)

const (
	minReadBuffer  = 1024 * 4 // 最小读缓存，4k
	minWriteBuffer = 1024 * 4 // 最小写缓存，4k
)

type rBuf struct {
	sync.RWMutex
	data    list.List
	buf     []byte
	sn      uint32
	minSN   uint32
	maxSN   uint32
	size    int
	timeout time.Time
	enable  chan struct{}
	msgACK  // 消息缓存
	message
}

type wBuf struct {
	sync.RWMutex
	data    list.List
	sn      uint32
	size    int
	timeout time.Time
	enable  chan struct{}
}

// 保存连接的基本信息，实现可靠性算法
type conn struct {
	sync.RWMutex               // 同步锁
	rBuf                       // 读缓存
	wBuf                       // 写缓存
	lAddr        *net.UDPAddr  // 本地地址
	rAddr        *net.UDPAddr  // 对端地址
	cToken       uint32        // token
	sToken       uint32        // token
	rtt          time.Duration // 实时RTT，用于数据补发判断
	rttVar       time.Duration // 平均RTT，用于计算RTO
}

func (this *conn) SetReadBuffer(n int) error {
	if n < minReadBuffer {
		this.rCap = minReadBuffer
	} else {
		this.rCap = n
	}
	return nil
}

func (this *conn) SetWriteBuffer(n int) error {
	if n < minWriteBuffer {
		this.wCap = minWriteBuffer
	} else {
		this.wCap = n
	}
	return nil
}

func (this *conn) Read(b []byte) (int, error) {
	this.mutex.Lock()
	// 是否设置了超时
	if this.rto.IsZero() {
		//this.rBuf.cond.L.Lock()
	}
	this.mutex.Unlock()
	return 0, nil
}

func (this *conn) Write(b []byte) (int, error) {
	return 0, nil
}

func (this *conn) Close() error {
	return nil
}

func (this *conn) LocalAddr() net.Addr {
	return this.lAddr
}

func (this *conn) RemoteAddr() net.Addr {
	return this.rAddr
}

func (this *conn) SetDeadline(t time.Time) error {
	this.rto = t
	this.wto = t
	return nil
}

func (this *conn) SetReadDeadline(t time.Time) error {
	this.rto = t
	return nil
}

func (this *conn) SetWriteDeadline(t time.Time) error {
	this.wto = t
	return nil
}

func (this *conn) read(b []byte) int {
	//this.rBuf.cond.L.Lock()
	//this.rBuf.cond.L.Unlock()
	return 0
}

// 添加
func (this *conn) handleMsgData(conn *net.UDPConn, msg *message) error {
	this.rBuf.Lock()
	// 是否超过了接收缓存的最大序列号
	if msg.sn >= this.rBuf.maxSN {
		this.rBuf.Unlock()
		return nil
	}
	// 小于已经接收的序列号，是重复的数据包
	if msg.sn < this.rBuf.sn {
		// 响应ack
		this.writeAck(conn)
		this.rBuf.Unlock()
		return nil
	}
	this.rBuf.Unlock()
	return nil
}

func (this *conn) writeAck(conn *net.UDPConn) error {
	this.rBuf.msgACK.sn = this.rBuf.sn
	this.rBuf.msgACK.maxSN = this.rBuf.maxSN
	this.rBuf.msgACK.free = uint32(this.rBuf.size - this.rBuf.data.Len())
	this.rBuf.message.EncACK(&this.rBuf.msgACK)
	this.rBuf.message.Bytes()
}

// 添加
func (this *conn) handleMsgACK(conn *net.UDPConn, msg *msgACK) error {
	return nil
}
