package rudp

import (
	"encoding/binary"
	"net"
	"runtime"
	"sync"
	"time"
)

// 使用默认值创建一个具备cs能力的RUDP
func New(address string) (*RUDP, error) {
	var cfg Config
	cfg.Listen = address
	cfg.AcceptQueue = 128
	return NewWithConfig(&cfg)
}

// 使用配置值创建一个新的RUDP，server能力需要配置Config.AcceptQueue
func NewWithConfig(cfg *Config) (*RUDP, error) {
	// 解析地址
	addr, err := net.ResolveUDPAddr("udp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	// 绑定地址
	conn, err := net.ListenUDP(addr.Network(), addr)
	if err != nil {
		return nil, err
	}
	// 初始化成员变量
	p := new(RUDP)
	p.valid = true
	p.conn = conn
	p.closed = make(chan struct{})
	if cfg.AcceptQueue > 0 {
		p.accepted = make(chan *Conn, cfg.AcceptQueue)
	}
	for i := 0; i < 2; i++ {
		p.token[i] = _rand.Uint32()
		p.connecting[i] = make(map[connKey]*Conn)
		p.connected[i] = make(map[connKey]*Conn)
	}
	p.connBuffer.r = uint32(defaultInt(cfg.ConnReadBuffer, defaultReadBuffer))
	p.connBuffer.w = uint32(defaultInt(cfg.ConnWriteBuffer, defaultWriteBuffer))
	p.connRTO = time.Duration(defaultInt(cfg.ConnRTO, defaultConnectRTO)) * time.Millisecond
	p.connMinRTO = time.Duration(maxInt(0, cfg.ConnMinRTO)) * time.Millisecond
	p.connMaxRTO = time.Duration(maxInt(0, cfg.ConnMaxRTO)) * time.Millisecond
	for i := 0; i < defaultInt(cfg.UDPDataRoutine, runtime.NumCPU()); i++ {
		go p.handleUDPDataRoutine()
	}
	return p, nil
}

// RUDP是一个同时具备C/S能力的引擎
type RUDP struct {
	valid      bool                 // 是否有效
	conn       *net.UDPConn         // 底层socket
	wait       sync.WaitGroup       // 等待所有协程退出
	closed     chan struct{}        // 通知所有协程退出的信号
	ioBytes    uint64RW             // udp读写的总字节
	accepted   chan *Conn           // 服务端已经建立，等待应用层处理的连接
	lock       [2]sync.RWMutex      // 客户端/服务端的锁
	token      [2]uint32            // 客户端/服务端的其实随机数
	connecting [2]map[connKey]*Conn // 客户端/服务端，正在建立连接的Conn
	connected  [2]map[connKey]*Conn // 客户端/服务端，已经建立连接的Conn
	connBuffer uint32RW             // Conn读写缓存的默认大小
	connRTO    time.Duration        // 客户端/服务端，建立连接的消息超时重发
	connMinRTO time.Duration        // Conn最小rto
	connMaxRTO time.Duration        // Conn最大rto
}

// 返回net.OpError对象
func (this *RUDP) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: nil,
		Addr:   this.conn.LocalAddr(),
		Err:    err,
	}
}

// net.Listener接口
func (this *RUDP) Close() error {
	// 关闭底层Conn
	err := this.conn.Close()
	if err != nil {
		return err
	}
	// 通知所有协程退出
	close(this.closed)
	close(this.accepted)
	// 关闭所有的Conn
	for i := 0; i < 2; i++ {
		for _, v := range this.connecting[i] {
			v.Close()
		}
		for _, v := range this.connected[i] {
			v.Close()
		}
	}
	// 等待所有协程退出
	this.wait.Wait()
	return nil
}

// net.Listener接口
func (this *RUDP) Accept() (net.Conn, error) {
	// 不作为服务端
	if this.accepted == nil {
		return nil, this.netOpError("listen", opErr("rudp is no a server"))
	}
	select {
	case conn, ok := <-this.accepted: // 等待新的连接
		if ok {
			return conn, nil
		}
	case <-this.closed: // RUDP关闭信号
	}
	return nil, this.netOpError("accept", closeErr("rudp"))
}

// net.Listener接口
func (this *RUDP) Addr() net.Addr {
	return this.conn.LocalAddr()
}

// 使用net.UDPConn向指定地址发送指定数据
func (this *RUDP) WriteTo(data []byte, addr *net.UDPAddr) (int, error) {
	n, err := this.conn.WriteToUDP(data, addr)
	if err == nil {
		this.ioBytes.w += uint64(n)
	}
	return n, err
}

// 使用net.UDPConn向指定地址发送指定数据
func (this *RUDP) WriteToConn(data []byte, conn *Conn) (int, error) {
	n, err := this.conn.WriteToUDP(data, conn.rIAddr)
	if err == nil {
		conn.rBuf.bytes += uint64(n)
		this.ioBytes.w += uint64(n)
	}
	return n, err
}

// 返回net.UDPConn的本地地址
func (this *RUDP) LocalAddr() net.Addr {
	return this.conn.LocalAddr()
}

// 读取的总字节，不包括ip和udp头
func (this *RUDP) ReadBytes() uint64 {
	return this.ioBytes.r
}

// 发送的总字节，不包括ip和udp头
func (this *RUDP) WriteBytes() uint64 {
	return this.ioBytes.r
}

// 读udp数据的协程
func (this *RUDP) handleUDPDataRoutine() {
	this.wait.Add(1)
	defer func() {
		this.wait.Done()
	}()
	var err error
	var n int
	var buf [maxMSS]byte
	var addr *net.UDPAddr
	for this.valid {
		// 读取数据并处理
		n, addr, err = this.conn.ReadFromUDP(buf[:])
		if err != nil {
			// todo: 日志
			continue
		}
		this.ioBytes.r += uint64(n)
		// 处理
		switch buf[msgType] {
		case msgDial:
			this.handleMsgDial(buf[:], addr)
			//case msgAccept: // s->c，接受连接，握手2
			//	this.handleMsgAccept(msg)
			//case msgRefuse: // s->c，拒绝连接，握手2
			//	this.handleMsgRefuse(msg)
			//case msgConnect: // c->s，收到接受连接的消息，握手3
			//	this.handleMsgConnect(msg)
			//case msgDataC: // c->s，数据
			//	this.handleMsgData(serverConn, msg)
			//case msgDataS: // s->c，数据
			//	this.handleMsgData(clientConn, msg)
			//case msgAckC: // c->s，收到数据确认
			//	this.handleMsgAck(serverConn, msg)
			//case msgAckS: // s->c，收到数据确认
			//	this.handleMsgAck(clientConn, msg)
			//case msgPing: // c->s
			//	this.handleMsgPing(msg)
			//case msgPong: // s->c
			//	this.handleMsgPong(msg)
			//case msgInvalidC: // c<->s，无效的连接
			//	this.handleMsgInvalid(serverConn, msg)
			//case msgInvalidS: // c<->s，无效的连接
			//	this.handleMsgInvalid(clientConn, msg)
		}
	}
}

// 关闭并释放Conn的资源
func (this *RUDP) freeConn(conn *Conn) {
	var key connKey
	key.Init(conn.rIAddr)
	// 移除connecting
	key.token = conn.wBuf.cToken
	this.lock[conn.cs].Lock()
	delete(this.connecting[conn.cs], key)
	// 移除connected
	key.token = conn.wBuf.sToken
	delete(this.connected[conn.cs], key)
	this.lock[conn.cs].Unlock()
	// 释放资源
	conn.release()
}

// 连接指定的地址
func (this *RUDP) Dial(addr string, timeout time.Duration) (*Conn, error) {
	// 新的客户端Conn
	conn, err := this.newDialConn(addr)
	if err != nil {
		return nil, err
	}
	// msgDial循环
	var buf [maxMSS]byte
	conn.writeMsgDial(buf[:])
	conn.timer.Reset(0)
	for err != nil {
		select {
		case now := <-conn.timer.C: // 超时重发
			if now.Sub(conn.rBuf.time) >= timeout {
				err = conn.netOpError("dial", opErr("timeout"))
			} else {
				this.WriteToConn(buf[:msgDialLength], conn)
				conn.timer.Reset(conn.wBuf.rto)
			}
		case state := <-conn.connected: // 建立连接结果通知
			switch state {
			case connStateConnect: // 已经建立连接
				return conn, nil
			case connStateDial: // 服务端拒绝连接
				err = conn.netOpError("dial", opErr("connect refuse"))
			//case connStateClosing,connStateAccept,connStateClose:
			default: // 其他状态都是逻辑bug
				err = conn.netOpError("dial", opErr("code bug"))
			}
		case <-this.closed: // RUDP被关闭
			err = conn.netOpError("dial", closeErr("rudp"))
		}
	}
	this.freeConn(conn)
	return nil, err
}

// 创建一个新的Conn变量
func (this *RUDP) newConn(cs uint8, state int, rAddr *net.UDPAddr) *Conn {
	p := new(Conn)
	p.cs = cs
	p.state = state
	p.connected = make(chan int, 1)
	p.closed = make(chan struct{})
	p.lLAddr = this.conn.LocalAddr().(*net.UDPAddr)
	p.rIAddr = rAddr
	return p
}

// 创建一个新的客户端Conn
func (this *RUDP) newDialConn(addr string) (*Conn, error) {
	rAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	var key connKey
	key.Init(rAddr)
	this.lock[clientConn].Lock()
	// 连接耗尽
	if len(this.connecting[clientConn]) >= maxConn {
		this.lock[clientConn].Unlock()
		return nil, this.netOpError("dial", opErr("too many connections"))
	}
	conn := this.newConn(clientConn, connStateDial, rAddr)
	conn.wBuf = newWriteBuffer(DetectMSS(rAddr), this.connBuffer.w)
	// 检查一个没有使用的token
	for this.valid {
		key.token = this.token[clientConn]
		this.token[clientConn]++
		_, ok := this.connecting[clientConn][key]
		if !ok {
			conn.wBuf.cToken = key.token
			this.connecting[clientConn][key] = conn
			break
		}
	}
	this.lock[clientConn].Unlock()
	return conn, nil
}

// 创建一个新的服务端Conn
func (this *RUDP) newAcceptConn(token uint32, cAddr *net.UDPAddr) *Conn {
	var key connKey
	key.token = token
	key.Init(cAddr)
	// 检查accepting Conn
	this.lock[serverConn].Lock()
	conn, ok := this.connecting[serverConn][key]
	// 存在
	if ok {
		this.lock[serverConn].Unlock()
		return conn
	}
	// 连接耗尽
	if len(this.connecting[serverConn]) >= maxConn {
		this.lock[serverConn].Unlock()
		return nil
	}
	conn = this.newConn(serverConn, connStateAccept, cAddr)
	conn.wBuf.cToken = token
	this.connecting[serverConn][key] = conn
	for this.valid {
		key.token = this.token[serverConn]
		this.token[serverConn]++
		_, ok := this.connected[serverConn][key]
		if !ok {
			conn.wBuf.sToken = key.token
			this.connected[serverConn][key] = conn
			break
		}
	}
	return conn
}

// 客户端Conn写协程
func (this *RUDP) clientConnRoutine(conn *Conn) {
	this.wait.Add(1)
	defer func() {
		this.freeConn(conn)
		this.wait.Done()
	}()
	this.connWriteLoop(conn)
}

// 服务端Conn写协程
func (this *RUDP) serverConnRoutine(conn *Conn, timeout time.Duration) {
	this.wait.Add(1)
	// 超时计时器
	defer func() {
		this.freeConn(conn)
		this.wait.Done()
	}()
	// 发送msgAccept
	var buf [maxMSS]byte
	conn.writeMsgAccept(buf[:])
	conn.timer.Reset(0)
	for {
		select {
		case <-this.closed: // RUDP关闭信号
			return
		case <-conn.closed: // Conn关闭信号
			return
		case now := <-conn.timer.C: // 超时重发
			if now.Sub(conn.rBuf.time) >= timeout {
				return
			}
			this.WriteToConn(buf[:msgAckLength], conn)
			conn.timer.Reset(conn.wBuf.rto)
		case <-conn.connected: // Conn连接通知
			this.connWriteLoop(conn)
			return
		}
	}
}

// 客户端Conn写协程
func (this *RUDP) connWriteLoop(conn *Conn) {
	for {
		select {
		case <-this.closed: // RUDP关闭信号
			return
		case <-conn.closed: // Conn关闭信号
			return
		case now := <-conn.timer.C: // 超时重传检查
			conn.timer.Reset(conn.wBuf.rto)
			conn.wBuf.Lock()
			conn.wBuf.WriteToUDP(func(data []byte) {
				this.WriteToConn(data, conn)
			}, now)
			conn.wBuf.Unlock()
		}
	}
}

// 处理msgDial
func (this *RUDP) handleMsgDial(buf []byte, addr *net.UDPAddr) {
	// 没有开启服务
	if this.accepted == nil {
		return
	}
	// 消息大小和版本
	if len(buf) != msgDialLength || buf[msgDialVersion] != msgVersion {
		return
	}
	// 客户端token
	cToken := binary.BigEndian.Uint32(buf[msgCToken:])
	// accepting Conn
	conn := this.newAcceptConn(cToken, addr)
	if conn == nil {
		// 响应msgRefuse
		buf[msgType] = msgRefuse
		binary.BigEndian.PutUint32(buf[msgRefuseToken:], cToken)
		this.WriteTo(buf[:msgRefuseLength], addr)
		return
	}
	conn.readMsgDial(buf)
	// 启动服务端Conn写协程
	go this.serverConnRoutine(conn, time.Duration(binary.BigEndian.Uint64(buf[msgDialTimeout:])))
}

// 处理msgAccept
func (this *RUDP) handleMsgAccept(buf []byte, addr *net.UDPAddr) {
	// 检查消息大小和版本
	if len(buf) != msgDialLength || buf[msgDialVersion] != msgVersion {
		return
	}
	// Conn
	var key connKey
	key.Init(addr)
	key.token = binary.BigEndian.Uint32(buf[msgAcceptCToken:])
	this.lock[clientConn].RLock()
	conn, ok := this.connecting[clientConn][key]
	this.lock[clientConn].RUnlock()
	if !ok {
		// 发送msgInvalid，通知对方不要在发送msgAccept了
		this.writeMsgInvalid(clientConn, binary.BigEndian.Uint32(buf[msgAcceptSToken:]), buf, addr)
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateDial:
		// 保存sToken
		conn.wBuf.sToken = binary.BigEndian.Uint32(buf[msgAcceptSToken:])
		// 修改状态
		conn.state = connStateConnect
		// 计算rto
		conn.wBuf.rtt = time.Now().Sub(conn.rBuf.time)
		conn.lock.Unlock()
		// connKey
		key.token = conn.wBuf.sToken
		// 添加到connected列表
		this.lock[clientConn].Lock()
		this.connected[clientConn][key] = conn
		this.lock[clientConn].Unlock()
		// 读取消息数据
		conn.readMsgAccept(buf)
		// 通知连接
		conn.connected <- 1
		// 启动客户端Conn写协程
		go this.clientConnRoutine(conn)
	case connStateConnect:
		// 已经是连接状态，说明是重复的消息
		conn.lock.Unlock()
	default:
		// 其他状态不处理
		conn.lock.Unlock()
		// 发送msgInvalid，通知对方不要在发送msgAccept了
		this.writeMsgInvalid(clientConn, binary.BigEndian.Uint32(buf[msgAcceptSToken:]), buf, addr)
		return
	}
	// 发送msgConnect
	conn.writeMsgConnect(buf)
	this.WriteToConn(buf[:msgConnectLength], conn)
}

// 处理msgRefuse
func (this *RUDP) handleMsgRefuse(buf []byte, addr *net.UDPAddr) {
	//// 检查消息大小和版本
	//if msg.len != msgRefuseLength || msg.buf[msgRefuseVersion] != msgVersion {
	//	return
	//}
	//// 客户端token
	//cToken := binary.BigEndian.Uint32(msg.buf[msgRefuseToken:])
	//// 检查是否有dialing Conn
	//this.lock[clientConn].RLock()
	//conn, ok := this.dialing[cToken]
	//this.lock[clientConn].RUnlock()
	//// 不存在不处理
	//if !ok {
	//	return
	//}
	//// 检查状态
	//conn.lock.Lock()
	//switch conn.state {
	//case connStateDial:
	//	conn.lock.Unlock()
	//	// 通知
	//	conn.connectSignal <- 1
	//default:
	//	// 其他状态不处理
	//	// connStateAccept: 客户端不可能
	//	// connStateClose: 正在关闭
	//	// connStateConnect: 已经连接
	//	conn.lock.Unlock()
	//}
}

// 处理msgConnect
func (this *RUDP) handleMsgConnect(buf []byte, addr *net.UDPAddr) {
	//// 没有开启服务
	//if this.accepted == nil {
	//	return
	//}
	//// 检查消息大小
	//if msg.len != msgConnectLength {
	//	return
	//}
	//// connKey
	//var key connKey
	//msg.InitConnKey(&key)
	//// 检查是否有accepting Conn
	//this.lock[serverConn].RLock()
	//conn, ok := this.accepting[key]
	//this.lock[serverConn].RUnlock()
	//// 不存在不处理
	//if !ok {
	//	return
	//}
	//// 检查状态
	//conn.lock.Lock()
	//switch conn.state {
	//case connStateAccept:
	//	// 修改状态
	//	conn.state = connStateConnect
	//	// 计算rto
	//	conn.calcRTO(time.Now().Sub(conn.time))
	//	conn.lock.Unlock()
	//	// 添加到connected列表
	//	this.lock[serverConn].Lock()
	//	this.connected[serverConn][key] = conn
	//	this.lock[serverConn].Unlock()
	//	// 通知
	//	conn.connectSignal <- 1
	//default:
	//	// 其他不处理
	//	// connStateConnect，重复消息
	//	// connStateDial，服务端不可能
	//	// connStateClose，关闭了
	//	conn.lock.Unlock()
	//}
}

// 处理msgPing
func (this *RUDP) handleMsgPing(buf []byte, addr *net.UDPAddr) {
	//if msg.len != msgPingLength {
	//	return
	//}
	//conn, ok := this.getConnectedConn(serverConn, msg)
	//if !ok {
	//	this.writeMsgInvalid(msg, msgInvalidC)
	//	return
	//}
	//conn.writeMsgPong(msg, binary.BigEndian.Uint32(msg.buf[msgPingId:]))
	//this.WriteToConn(msg.buf[:msgPongLength], conn)
}

// 处理msgPong
func (this *RUDP) handleMsgPong(buf []byte, addr *net.UDPAddr) {
	//conn, ok := this.getConnectedConn(clientConn, msg)
	//if ok {
	//	conn.readMsgPong(msg)
	//}
}

// 处理msgData
func (this *RUDP) handleMsgData(cs uint8, buf []byte, addr *net.UDPAddr) {
	//if msg.len <= msgDataPayload {
	//	return
	//}
	//conn, ok := this.getConnectedConn(cs, msg)
	//if !ok {
	//	this.writeMsgInvalid(msg, cs)
	//	return
	//}
	//conn.readMsgData(msg)
	//this.WriteToConn(msg.buf[:msgAckLength], conn)
}

// 处理msgAck
func (this *RUDP) handleMsgAck(cs uint8, buf []byte, addr *net.UDPAddr) {
	//if msg.len != msgAckLength {
	//	return
	//}
	//conn, ok := this.getConnectedConn(cs, msg)
	//if ok {
	//	conn.readMsgAck(msg)
	//}
}

// 处理msgInvalid
func (this *RUDP) handleMsgInvalid(cs uint8, buf []byte, addr *net.UDPAddr) {
	//if msg.len != msgInvalidLength {
	//	return
	//}
	//conn, ok := this.getConnectedConn(cs, msg)
	//if ok {
	//	this.closeConn(conn)
	//}
}

// 发送msgInvalidC/msgInvalidS
func (this *RUDP) writeMsgInvalid(cs uint8, token uint32, buf []byte, addr *net.UDPAddr) {
	buf[msgType] = msgInvalid[cs]
	binary.BigEndian.PutUint32(buf[msgInvalidToken:], token)
	this.WriteTo(buf[:msgInvalidLength], addr)
}
