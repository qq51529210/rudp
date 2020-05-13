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
	// 初始化dial消息
	msg := _dataPool.Get().(*udpData)
	conn.initMsgDial(msg)
	// 超时重发计时器
	ticker := time.NewTicker(this.client.dialRTO)
	// 回收资源
	defer func() {
		ticker.Stop()
		_dataPool.Put(msg)
	}()
Loop:
	for {
		select {
		case now := <-ticker.C:
			// 超时
			if now.Sub(start_time) >= timeout {
				err = conn.netOpError("dial", opErr("timeout"))
				break Loop
			}
			// 重发
			this.writeToConn(msg, conn)
		case <-conn.connectSignal:
			switch conn.state {
			case connStateConnect:
				// 已经建立连接
				return conn, nil
			case connStateClose:
				// 服务端拒绝连接
				err = conn.netOpError("dial", opErr("connect refuse"))
			default:
				// 其他状态都是逻辑bug
				err = conn.netOpError("dial", opErr("bug"))
			}
			break Loop
		case <-this.closeSignal:
			// rudp关闭信号
			err = conn.netOpError("dial", closeErr("rudp"))
			break Loop
		}
	}
	// 出错
	close(conn.connectSignal)
	// 移除列表
	this.client.Lock()
	delete(this.client.dialing, conn.cToken)
	this.client.Unlock()
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
	// 探测mss，并初始化Conn
	conn := this.newConn(csC, connStateDial, rAddr)
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
			return nil, this.netOpError("dial", opErr("too many connections"))
		}
	}
	// 添加到列表
	this.client.dialing[conn.cToken] = conn
	this.client.Unlock()
	return conn, nil
}

// 客户端Conn写协程
func (this *RUDP) connWriteRoutine(conn *Conn) {
	this.wait.Add(1)
	timer := time.NewTimer(conn.wBuf.rto)
	defer func() {
		timer.Stop()
		this.closeConn(conn)
		this.wait.Done()
	}()
	select {
	case now := <-timer.C:
		// 超时重发检查
		this.checkWriteBuffer(conn, now)
	case <-this.closeSignal:
		// rudp关闭信号
		return
	case <-conn.closeSignal:
		// Conn关闭信号
		return
	}
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
