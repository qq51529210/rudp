package rudp

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// 发送dial消息的时间间隔
	_dialDuration = time.Millisecond * 100
)

// 设置发送dial消息的时间间隔
func SetDialDuration(duration time.Duration) {
	if duration > 0 {
		_dialDuration = duration
	}
}

type client struct {
	sync.RWMutex                    // 锁
	*net.UDPConn                    // socket
	dialingConn   map[dialKey]*Conn // 正在创建连接的Conn
	connectedConn map[uint32]*Conn  // 已经建立连接Conn
	dialToken     uint32            // 用于建立连接的随机数，递增
	valid         bool              // 是否
}

func newClient(conn *net.UDPConn) *client {
	p := new(client)
	p.UDPConn = conn
	p.dialingConn = make(map[dialKey]*Conn)
	p.connectedConn = make(map[uint32]*Conn)
	return p
}

// 连接指定的地址，返回
func (this *client) Dial(addr string, timeout time.Duration) (*Conn, error) {
	//conn, err := this.newDialConn(addr)
	//if err != nil {
	//	return nil, err
	//}
	//// 初始化消息
	//msg := new(message)
	//msg.EncDial()
	//// 开始时间
	//start_time := time.Now()
	//ticker := time.NewTicker(_dialDuration)
	//for {
	//	select {
	//	case now := <-ticker.C: // 到时间发送dial消息
	//		if now.Sub(start_time) >= timeout {
	//			// 超时
	//			this.removeDialConn(conn)
	//			return nil, this.opError("dial", conn.rAddr, errorTimeout)
	//		}
	//		// 检查Conn的状态
	//		switch conn.getState() {
	//		case connStateDialing: // 还未建立连接，继续发送消息
	//			_, err = this.conn.WriteToUDP(msg.Bytes(), conn.rAddr.(*net.UDPAddr))
	//			if err != nil {
	//				this.removeDialConn(conn)
	//				return nil, err
	//			}
	//		case connStateConnected: // 已经建立连接
	//			return conn, nil
	//		case connStateRefused: // 服务端拒绝连接
	//			this.removeDialConn(conn)
	//			return nil, this.opError("dial", conn.rAddr, errorDialRefused)
	//		default: // 其他状态
	//			this.removeDialConn(conn)
	//			return nil, this.opError("dial", conn.rAddr, errorClosed)
	//		}
	//	case <-this.quit: // 被关闭了
	//		this.removeDialConn(conn)
	//		return nil, this.opError("dial", conn.rAddr, errorClosed)
	//	}
	//}
	return nil, nil
}

// 创建一个新的Conn，添加到dialing列表中
func (this *client) newDialConn(addr string) (*Conn, error) {
	// 解析地址
	rAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	// 初始化Conn
	conn := newConn()
	conn.rAddr = rAddr
	conn.lAddr = this.LocalAddr()
	conn.state = connStateDialing
	// 当前随机数+1
	conn.cToken = atomic.AddUint32(&this.dialToken, 1)
	// 加入列表
	//token := conn.cToken
	//this.Lock()
	//for this.valid {
	//	// 是否存在
	//	_, ok := this.dialingConn[conn.cToken]
	//	if ok {
	//		conn.cToken = atomic.AddUint32(&this.client.dialToken, 1)
	//		// 循环了uint32一圈，列表满了
	//		if conn.cToken == token {
	//			return nil, this.opError("dial", rAddr, errorTooManyConn)
	//		}
	//	}
	//	this.client.dialConn[conn.cToken] = conn
	//	break
	//}
	//this.client.Unlock()
	return conn, nil
}

// 移除指定的Conn
func (this *client) removeDialConn(conn *Conn) {
	//this.Lock()
	//delete(this.dialConn, conn.cToken)
	//this.Unlock()
}
