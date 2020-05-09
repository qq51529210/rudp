package rudp

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

const (
	defaultDialRTO = time.Millisecond * 100 // 发送msgDial消息的时间间隔
)

type client struct {
	sync.RWMutex                  // 锁
	dialing      map[uint32]*Conn // 正在创建连接的Conn
	connected    map[uint32]*Conn // 已经建立连接Conn
	token        uint32           // 用于建立连接的随机数，递增
	dialRTO      time.Duration    // 发送msgDial消息的时间间隔
}

func newClient() *client {
	p := new(client)
	p.dialing = make(map[uint32]*Conn)
	p.connected = make(map[uint32]*Conn)
	p.token = _rand.Uint32()
	p.dialRTO = defaultDialRTO
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
	conn, token, err := this.newDialConn(addr)
	if err != nil {
		return nil, err
	}
	// 初始化msgDial
	var msg message
	this.encodeMsgDial(conn, &msg)
	// 开始时间
	start_time := time.Now()
	// 发送msgDial的时间间隔
	ticker := time.NewTicker(this.client.dialRTO)
	// 循环
Loop:
	for {
		select {
		case now := <-ticker.C:
			// 发送msgDial间隔
			if now.Sub(start_time) >= timeout {
				// 超时
				err = this.opError("dial", conn.rAddr, errOP("timeout"))
				break Loop
			}
			// 发送
			_, err = this.conn.WriteToUDP(msg.b[:msgDialLength], conn.rAddr)
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
				this.client.connected[conn.token] = conn
				this.client.Unlock()
				// 返回
				return conn, nil
			case connStateClose:
				// 服务端拒绝连接
				err = this.opError("dial", conn.rAddr, errOP("refuse"))
			default:
				// 其他状态都是逻辑bug
				err = this.opError("dial", conn.rAddr, errOP("bug"))
			}
			break Loop
		case <-this.closeSignal:
			// rudp被关闭
			err = this.opError("dial", conn.rAddr, errClosed)
			break Loop
		}
	}
	// 出错
	ticker.Stop()
	close(conn.connectSignal)
	// 移除列表
	this.client.Lock()
	delete(this.client.dialing, token)
	this.client.Unlock()
	// 返回
	return nil, err
}

// 创建一个新的客户端Conn，拆分Dial()代码
func (this *RUDP) newDialConn(addr string) (*Conn, uint32, error) {
	// 解析地址
	rAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, 0, err
	}
	// 初始化Conn
	conn := this.newConn(connStateDial, rAddr)
	// 探测mss
	conn.mss = DetectMSS(rAddr)
	// 产生客户端token
	this.client.Lock()
	token := this.client.token
	this.client.token++
	tkn := token
	ok := true
	for {
		_, ok = this.client.dialing[token]
		if !ok {
			break
		}
		token = this.client.token
		this.client.token++
		// 连接耗尽，2^32，理论上现有计算机不可能
		if conn.token == tkn {
			this.client.Unlock()
			return nil, 0, this.opError("dial", rAddr, errOP("too many connections"))
		}
	}
	// 添加到列表
	this.client.dialing[token] = conn
	this.client.Unlock()
	return conn, token, nil
}

// 编码msgDial，拆分Dial()代码
func (this *RUDP) encodeMsgDial(conn *Conn, msg *message) {
	msg.b[msgType] = msgDial
	binary.BigEndian.PutUint32(msg.b[msgDialVersion:], msgVersion)
	copy(msg.b[msgDialLocalIP:], conn.lAddr.IP.To16())
	binary.BigEndian.PutUint16(msg.b[msgDialLocalPort:], uint16(conn.lAddr.Port))
	binary.BigEndian.PutUint16(msg.b[msgDialMSS:], conn.mss)
	binary.BigEndian.PutUint32(msg.b[msgDialReadBuffer:], uint32(conn.rBuf.length))
	binary.BigEndian.PutUint32(msg.b[msgDialWriteBuffer:], uint32(conn.wBuf.length))
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
