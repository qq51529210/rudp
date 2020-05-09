package rudp

import (
	"net"
	"sync"
	"time"
)

type connState int

const (
	connStateClose   connState = iota // c<->s，连接超时/被应用层关闭，发送close消息
	connStateDial                     // c->s，发送dial消息
	connStateAccept                   // s->c，确认连接，发送accept消息
	connStateConnect                  // c->s，发送connect消息，双向确认连接
)

// 保存连接的基本信息，实现可靠性算法
type Conn struct {
	connState                    // 状态
	sync.RWMutex                 // 同步锁
	readBuffer                   // 读缓存
	writeBuffer                  // 写缓存
	rwBytes                      // 读写字节统计
	connectSignal chan struct{}  // 作为确定连接的信号
	closeSignal   chan struct{}  // 作为确定连接的信号
	waitQuit      sync.WaitGroup // 等待所有协程退出
	lAddr         *net.UDPAddr   // 本地地址
	rAddr         *net.UDPAddr   // 对端地址
	token         uint32         // 连接随机token，由server产生
	mss           uint16         // 每一个消息大小，包括消息头
	rto           time.Duration  // 超时重发
	rtt           time.Duration  // 实时RTT，用于数据补发判断
	rttVar        time.Duration  // 平均RTT，用于计算RTO
}

func (this *Conn) SetReadBuffer(n int) error {
	this.rBuf.Lock()
	this.rBuf.length = n / int(this.mss)
	if this.rBuf.length < 1 {
		this.rBuf.length = 1
	}
	this.rBuf.Unlock()
	return nil
}

func (this *Conn) SetWriteBuffer(n int) error {
	this.wBuf.Lock()
	this.wBuf.length = n / int(this.mss)
	if this.wBuf.length < 1 {
		this.wBuf.length = 1
	}
	this.wBuf.Unlock()
	return nil
}

func (this *Conn) Read(b []byte) (int, error) {
	this.mutex.Lock()
	// 是否设置了超时
	if this.rto.IsZero() {
		//this.rBuf.cond.L.Lock()
	}
	this.mutex.Unlock()
	return 0, nil
}

func (this *Conn) Write(b []byte) (int, error) {
	return 0, nil
}

func (this *Conn) Close() error {
	return nil
}

func (this *Conn) LocalAddr() net.Addr {
	return this.lAddr
}

func (this *Conn) RemoteAddr() net.Addr {
	return this.rAddr
}

func (this *Conn) SetDeadline(t time.Time) error {
	this.rto = t
	this.wto = t
	return nil
}

func (this *Conn) SetReadDeadline(t time.Time) error {
	this.rto = t
	return nil
}

func (this *Conn) SetWriteDeadline(t time.Time) error {
	this.wto = t
	return nil
}

func (this *Conn) read(b []byte) int {
	//this.rBuf.cond.L.Lock()
	//this.rBuf.cond.L.Unlock()
	return 0
}

func (this *Conn) checkWriteBuffer(conn *net.UDPConn, now time.Time) (int, error) {
}
