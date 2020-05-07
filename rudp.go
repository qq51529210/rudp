package rudp

import (
	"errors"
	"net"
	"sync"
	"time"
)

// 数据包端大小
const (
	Version     = 0                        // RUT版本
	IPV4Header  = 20                       // IP数据包头大小
	UDPV4Header = 8                        // UDP数据包头大小
	TransHeader = IPV4Header + UDPV4Header // UDP传输层的包头大小
	MinMTU      = 576                      // 链路最小的MTU
	MaxMTU      = 1500                     // 链路最大的MTU
	MinMSS      = MinMTU - TransHeader     // 应用层最小数据包
	MaxMSS      = MaxMTU - TransHeader     // 应用层最大数据包
)

// 是客户端还是服务端的数据
const (
	csClient = iota
	csServer
	csMax
)

var (
	errorTimeout = errors.New("timeout")
	errorClosed  = errors.New("closed")
)

// 监听指定的地址，返回RUDP对象
func Listen(address string) (*RUDP, error) {
	// 准备监听
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP(addr.Network(), addr)
	if err != nil {
		return nil, err
	}
	p := new(RUDP)
	p.conn = conn
	p.valid = true
	p.session[csClient] = make(map[uint64]*Conn)
	p.session[csServer] = make(map[uint64]*Conn)
	p.quit = make(chan struct{})
	p.dialDur = time.Millisecond * 100
	return p, nil
}

// 可以作为客户端，也可以作为服务端
type RUDP struct {
	conn    *net.UDPConn            // net.UDPConn
	session [csMax]map[uint64]*Conn // 已经连接的列表
	dialing map[dialKey]*Conn       // 正在创建连接的客户端
	wait    sync.WaitGroup          // 同步等待所有的goroutine
	mutex   sync.Mutex              // 同步锁
	valid   bool                    // 是否调用了Close()
	quit    chan struct{}           // 退出信息
	dialDur time.Duration           // dial消息发送的间隔
}

// 设置dial消息发送的间隔
func (this *RUDP) SetDialDuration(duration time.Duration) {
	this.dialDur = duration
}

// 使用net.UDPConn向指定地址发送指定数据
func (this *RUDP) WriteTo(b []byte, addr *net.UDPAddr) (int, error) {
	return this.conn.WriteToUDP(b, addr)
}

// 返回net.UDPConn的本地地址
func (this *RUDP) LocalAddr() net.Addr {
	return this.conn.LocalAddr()
}

// 主动连接指定的地址
// addr: 对端的udp地址
// to: 连接超时时间
func (this *RUDP) Dial(addr string, to time.Time) (*Conn, error) {
	// 解析地址
	rAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	// 
	for {
		select {
		case <-time.After(this.dialDur): // 到时间发送dial消息

		case <-time.After(to.Sub(time.Now())): // 超时
			return nil, &net.OpError{
				Op:     "dial",
				Net:    "udp",
				Source: this.LocalAddr(),
				Addr:   rAddr,
				Err:    errorTimeout,
			}
		case <-this.quit: // 被关闭了
			return nil, &net.OpError{
				Op:     "dial",
				Net:    "udp",
				Source: this.LocalAddr(),
				Addr:   rAddr,
				Err:    errorClosed,
			}
		}
	}
	// 虚拟对象
	conn := new(Conn)
	conn.rAddr = rAddr
	conn.lAddr = this.conn.LocalAddr()
	return conn, nil
}

// 阻塞，等新的连接
func (this *RUDP) Accept() (*Conn, error) {
	return nil, nil
}

// 关闭
func (this *RUDP) Close() error {
	return this.conn.Close()
}

// 读数据的循环
func (this *RUDP) readLoop() {
	for {
		this.conn.ReadFromUDP()
	}
}
