package rudp

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

type client struct {
	sync.RWMutex                  // 锁
	dialing      map[uint32]*Conn // 正在创建的连接
	connected    map[uint32]*Conn // 已经建立的连接
	token        uint32           // 用于建立连接的随机数，递增
	dialRTO      time.Duration    // dial消息超时重发
}

func newClient(cfg *Config) *client {
	p := new(client)
	p.dialing = make(map[uint32]*Conn)
	p.connected = make(map[uint32]*Conn)
	p.token = _rand.Uint32()
	p.dialRTO = time.Duration(defaultInt(cfg.DialRTO, 100)) * time.Millisecond
	return p
}

// 设置msgDial的超时重发
func (this *RUDP) SetDialRTO(rto time.Duration) {
	if rto > 0 {
		this.client.dialRTO = rto
	}
}

// 连接指定的地址，返回
func (this *RUDP) Dial(addr string, timeout time.Duration) (*Conn, error) {
	// 开始时间
	start_time := time.Now()
	// 创建Conn
	conn, err := this.dialConn(addr)
	if err != nil {
		return nil, err
	}
	// dial消息
	msg := _msgPool.Get().(*message)
	msg.b[msgType] = msgDial
	binary.BigEndian.PutUint32(msg.b[msgDialVersion:], msgVersion)
	copy(msg.b[msgDialLocalIP:], conn.lAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.b[msgDialLocalPort:], uint16(conn.lAddr.Port))
	binary.BigEndian.PutUint16(msg.b[msgDialMSS:], conn.mss)
	binary.BigEndian.PutUint32(msg.b[msgDialReadBuffer:], conn.readBuffer.Free())
	binary.BigEndian.PutUint32(msg.b[msgDialWriteBuffer:], conn.writeBuffer.Free())
	msg.a = conn.rAddr
	// 超时重发计时器
	ticker := time.NewTicker(this.client.dialRTO)
Loop:
	for {
		select {
		case now := <-ticker.C:
			// 超时重发
			if now.Sub(start_time) >= timeout {
				err = conn.netError("dial", errOP("timeout"))
				break Loop
			}
			err = this.writeMessageToConn(msg.b[:msgDialLength], msg.a, conn)
			if err != nil {
				break Loop
			}
		case <-conn.connectSignal:
			// 有结果通知
			conn.RLock()
			state := conn.connState
			conn.RUnlock()
			// 检查状态
			switch state {
			case connStateConnect:
				// 已经建立连接
				ticker.Stop()
				// 添加到client.connected
				this.client.Lock()
				this.client.connected[conn.sToken] = conn
				this.client.Unlock()
				// 返回
				return conn, nil
			case connStateClose:
				// 服务端拒绝连接
				err = conn.netError("dial", errOP("rudp"))
			default:
				// 其他状态都是逻辑bug
				err = conn.netError("dial", errOP("bug"))
			}
			break Loop
		case <-this.closeSignal:
			// rudp关闭信号
			err = conn.netError("dial", errClosed("rudp"))
			break Loop
		}
	}
	// 出错
	// 返回
	return nil, err
}

// 创建一个新的客户端Conn，拆分Dial()代码
func (this *RUDP) dialConn(addr string) (*Conn, error) {
	// 解析地址
	rAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	// 初始化Conn
	conn := this.newConn(connStateDial, rAddr)
	// 探测mss
	conn.mss = DetectMSS(rAddr)
	// 产生客户端token
	this.client.Lock()
	conn.cToken = this.client.token
	this.client.token++
	tkn := conn.cToken
	ok := true
	for {
		_, ok = this.client.dialing[conn.cToken]
		if !ok {
			break
		}
		conn.cToken = this.client.token
		this.client.token++
		// 连接耗尽，2^32，理论上现有计算机不可能
		if conn.cToken == tkn {
			this.client.Unlock()
			return nil, this.opError("dial", rAddr, errOP("too many connections"))
		}
	}
	// 添加到列表
	this.client.dialing[conn.cToken] = conn
	this.client.Unlock()
	return conn, nil
}

// 编码dial消息，拆分Dial()代码
func (this *RUDP) dialMsg(ticker *time.Ticker, conn *Conn) {
	ticker.Stop()
	close(conn.connectSignal)
	// 移除列表
	this.client.Lock()
	delete(this.client.dialing, conn.cToken)
	this.client.Unlock()
}

// 处理Dial()错误，拆分Dial()代码
func (this *RUDP) dialError(ticker *time.Ticker, conn *Conn) {
	ticker.Stop()
	close(conn.connectSignal)
	// 移除列表
	this.client.Lock()
	delete(this.client.dialing, conn.cToken)
	this.client.Unlock()
}

// 释放client相关的资源
func (this *RUDP) closeClient() {
	for _, c := range this.client.dialing {
		c.Close()
	}
	for _, c := range this.client.connected {
		c.Close()
	}
}
