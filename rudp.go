package rudp

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
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
	this.valid = false
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
		conn.ioBytes.w += uint64(n)
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
	var buf = make([]byte, maxMSS)
	var addr *net.UDPAddr
	for this.valid {
		// 读取数据并处理
		n, addr, err = this.conn.ReadFromUDP(buf)
		if err != nil {
			// todo: 日志
			continue
		}
		this.ioBytes.r += uint64(n)
		// 处理
		switch buf[msgType] {
		case msgDial:
			this.handleMsgDial(buf, n, addr)
		case msgAccept:
			this.handleMsgAccept(buf, n, addr)
		case msgRefuse:
			this.handleMsgRefuse(buf, n, addr)
		case msgConnect:
			this.handleMsgConnect(buf, n, addr)
		case msgDataC:
			this.handleMsgData(serverConn, buf, n, addr)
		case msgDataS:
			this.handleMsgData(clientConn, buf, n, addr)
		case msgAckC:
			this.handleMsgAck(serverConn, buf, n, addr)
		case msgAckS:
			this.handleMsgAck(clientConn, buf, n, addr)
		case msgPing:
			this.handleMsgPing(buf, n, addr)
		case msgPong:
			this.handleMsgPong(buf, n, addr)
		case msgInvalidC:
			this.handleMsgInvalid(serverConn, buf, n, addr)
		case msgInvalidS:
			this.handleMsgInvalid(clientConn, buf, n, addr)
		}
	}
}

// 连接指定的地址
func (this *RUDP) Dial(addr string, timeout time.Duration) (*Conn, error) {
	// 新的客户端Conn
	conn, err := this.newDialConn(addr)
	if err != nil {
		return nil, err
	}
	// 发送msgDial
	var buf [maxMSS]byte
	conn.writeMsgDial(buf[:], timeout)
	conn.timer.Reset(0)
	for err == nil {
		select {
		case now := <-conn.timer.C: // 超时重发
			if now.Sub(conn.readTime) >= timeout {
				err = conn.netOpError("dial", opErr("timeout"))
			} else {
				this.WriteToConn(buf[:msgDialLength], conn)
				conn.timer.Reset(conn.rto)
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
func (this *RUDP) newConn(cs connCS, state connState, rAddr *net.UDPAddr) *Conn {
	p := new(Conn)
	p.cs = cs
	p.state = state
	p.connected = make(chan connState, 1)
	p.closed = make(chan struct{})
	p.timer = time.NewTimer(0)
	p.lLAddr = this.conn.LocalAddr().(*net.UDPAddr)
	p.lIAddr = new(net.UDPAddr)
	p.rLAddr = new(net.UDPAddr)
	p.rIAddr = rAddr
	p.readQueue = readDataPool.Get().(*readData)
	p.readQueue.next = nil
	p.readable = make(chan int, 1)
	p.readable <- 1
	p.readTime = time.Now()
	p.writeQueueHead = writeDataPool.Get().(*writeData)
	p.writeQueueHead.next = nil
	p.writeQueueTail = p.writeQueueHead
	p.writeable = make(chan int, 1)
	p.writeable <- 1
	p.rto = this.connRTO
	p.minRTO = this.connMinRTO
	p.maxRTO = this.connMaxRTO
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
	// 检查一个没有使用的token
	for this.valid {
		key.token = this.token[clientConn]
		this.token[clientConn]++
		_, ok := this.connecting[clientConn][key]
		if !ok {
			conn.cToken = key.token
			this.connecting[clientConn][key] = conn
			break
		}
	}
	this.lock[clientConn].Unlock()
	// mss
	conn.writeMSS = DetectMSS(rAddr)
	conn.writeQueueMaxLen = calcMaxLen(this.connBuffer.w, conn.writeMSS)
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
	this.connecting[serverConn][key] = conn
	//
	conn.cToken = token
	this.connecting[serverConn][key] = conn
	for this.valid {
		key.token = this.token[serverConn]
		this.token[serverConn]++
		_, ok := this.connected[serverConn][key]
		if !ok {
			conn.sToken = key.token
			this.connected[serverConn][key] = conn
			break
		}
	}
	this.lock[serverConn].Unlock()
	// 探测mss，并根据设定的缓存，确定发送队列的大小
	conn.writeMSS = DetectMSS(cAddr)
	conn.writeQueueMaxLen = calcMaxLen(this.connBuffer.w, conn.writeMSS)
	return conn
}

// 关闭并释放Conn的资源
func (this *RUDP) freeConn(conn *Conn) {
	conn.lock.Lock()
	if conn.state == connStateClose {
		conn.lock.Unlock()
		return
	}
	conn.state = connStateClose
	conn.lock.Unlock()
	// 移除
	var key connKey
	key.Init(conn.rIAddr)
	// 移除connecting
	key.token = conn.cToken
	this.lock[conn.cs].Lock()
	delete(this.connecting[conn.cs], key)
	// 移除connected
	key.token = conn.sToken
	delete(this.connected[conn.cs], key)
	this.lock[conn.cs].Unlock()
	// 释放资源
	close(conn.closed)
	close(conn.connected)
	close(conn.readable)
	close(conn.writeable)
	conn.timer.Stop()
	r := conn.readQueue
	for r != nil {
		n := r.next
		readDataPool.Put(r)
		r = n
	}
	w := conn.writeQueueHead
	for w != nil {
		n := w.next
		writeDataPool.Put(w)
		w = n
	}
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
	// msgAccept
	var buf [maxMSS]byte
	conn.writeMsgAccept(buf[:])
	// 发送
	conn.timer.Reset(0)
	for {
		select {
		case <-this.closed: // RUDP关闭信号
			return
		case <-conn.closed: // Conn关闭信号
			return
		case now := <-conn.timer.C: // 超时重发
			if now.Sub(conn.readTime) >= timeout {
				return
			}
			this.WriteToConn(buf[:msgAcceptLength], conn)
			conn.timer.Reset(conn.rto)
		case <-conn.connected: // Conn连接通知
			this.accepted <- conn
			this.connWriteLoop(conn)
			return
		}
	}
}

// 获取connecting Conn
func (this *RUDP) connectingConn(cs uint8, token uint32, addr *net.UDPAddr) *Conn {
	var key connKey
	key.Init(addr)
	key.token = token
	this.lock[cs].RLock()
	conn, _ := this.connecting[cs][key]
	this.lock[cs].RUnlock()
	return conn
}

// 获取connected Conn
func (this *RUDP) connectedConn(cs connCS, token uint32, addr *net.UDPAddr) *Conn {
	var key connKey
	key.Init(addr)
	key.token = token
	this.lock[cs].RLock()
	conn, _ := this.connected[cs][key]
	this.lock[cs].RUnlock()
	return conn
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
			conn.writeLock.Lock()
			conn.writeToUDP(func(data []byte) {
				this.WriteToConn(data, conn)
			}, now)
			conn.writeLock.Unlock()
			conn.timer.Reset(conn.rto)
			//fmt.Fprintf(os.Stdout, "rto<%v>"+
			//	"\n",
			//	conn.rto,
			//)
		}
	}
}

// 处理msgDial
func (this *RUDP) handleMsgDial(buf []byte, n int, addr *net.UDPAddr) {
	// 消息大小和版本
	if n != msgDialLength || binary.BigEndian.Uint32(buf[msgDialVersion:]) != msgVersion {
		return
	}
	// 客户端token
	cToken := binary.BigEndian.Uint32(buf[msgToken:])
	// 没有开启服务
	if this.accepted == nil {
		buf[msgType] = msgRefuse
		binary.BigEndian.PutUint32(buf[msgRefuseToken:], cToken)
		this.WriteTo(buf[:msgRefuseLength], addr)
		return
	}
	// accepting Conn
	conn := this.newAcceptConn(cToken, addr)
	if conn == nil {
		// 响应msgRefuse
		buf[msgType] = msgRefuse
		binary.BigEndian.PutUint32(buf[msgRefuseToken:], cToken)
		this.WriteTo(buf[:msgRefuseLength], addr)
		return
	}
	conn.readMsgDial(buf, this.connBuffer.r)
	// 启动服务端Conn写协程
	go this.serverConnRoutine(conn, time.Duration(binary.BigEndian.Uint64(buf[msgDialTimeout:])))
	fmt.Fprintf(os.Stdout, "msg-dial: "+
		"token<%d>"+
		"version<%d>"+
		"remote listen address<%s>"+
		"local internet address<%s>"+
		"mss<%d>"+
		"timeout<%v>"+
		"\n",
		conn.cToken,
		binary.BigEndian.Uint32(buf[msgDialVersion:]),
		conn.rLAddr.String(),
		conn.lIAddr.String(),
		conn.readMSS,
		time.Duration(binary.BigEndian.Uint64(buf[msgDialTimeout:])),
	)
}

// 处理msgAccept
func (this *RUDP) handleMsgAccept(buf []byte, n int, addr *net.UDPAddr) {
	// 检查消息大小和版本
	if n != msgAcceptLength || binary.BigEndian.Uint32(buf[msgAcceptVersion:]) != msgVersion {
		return
	}
	// connecting Conn
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
		conn.sToken = binary.BigEndian.Uint32(buf[msgAcceptSToken:])
		// 修改状态
		conn.state = connStateConnect
		// 计算rto
		conn.rtt = time.Now().Sub(conn.readTime)
		conn.lock.Unlock()
		// connKey
		key.token = conn.sToken
		// 添加到connected列表
		this.lock[clientConn].Lock()
		this.connected[clientConn][key] = conn
		this.lock[clientConn].Unlock()
		// 读取消息数据
		conn.readMsgAccept(buf, this.connBuffer.r)
		// 通知连接
		conn.connected <- connStateConnect
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
	fmt.Fprintf(os.Stdout, "msg-accept: "+
		"ctoken<%d>"+
		"stoken<%d>"+
		"version<%d>"+
		"remote listen address<%s>"+
		"local internet address<%s>"+
		"mss<%d>"+
		"\n",
		conn.cToken,
		conn.sToken,
		binary.BigEndian.Uint32(buf[msgAcceptVersion:]),
		conn.rLAddr.String(),
		conn.lIAddr.String(),
		conn.readMSS,
	)
}

// 处理msgRefuse
func (this *RUDP) handleMsgRefuse(buf []byte, n int, addr *net.UDPAddr) {
	// 检查消息大小和版本
	if n != msgRefuseLength || binary.BigEndian.Uint32(buf[msgRefuseVersion:]) != msgVersion {
		return
	}
	// connecting Conn
	var key connKey
	key.Init(addr)
	key.token = binary.BigEndian.Uint32(buf[msgRefuseToken:])
	this.lock[clientConn].RLock()
	conn, ok := this.connecting[clientConn][key]
	this.lock[clientConn].RUnlock()
	// 不存在不处理
	if !ok {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateDial:
		conn.lock.Unlock()
		// 通知
		conn.connected <- connStateDial
	default:
		// 其他状态不处理
		// connStateAccept: 客户端不可能
		// connStateClose: 正在关闭
		// connStateConnect: 已经连接
		conn.lock.Unlock()
	}
	fmt.Fprintf(os.Stdout, "msg-refuse: "+
		"token<%d>"+
		"version<%d>"+
		"\n",
		key.token,
		binary.BigEndian.Uint32(buf[msgRefuseVersion:]),
	)
}

// 处理msgConnect
func (this *RUDP) handleMsgConnect(buf []byte, n int, addr *net.UDPAddr) {
	// 没有开启服务
	if this.accepted == nil {
		return
	}
	// 检查消息大小
	if n != msgConnectLength {
		return
	}
	// Conn
	var key connKey
	key.Init(addr)
	key.token = binary.BigEndian.Uint32(buf[msgConnectToken:])
	this.lock[serverConn].RLock()
	conn, ok := this.connected[serverConn][key]
	this.lock[serverConn].RUnlock()
	// 不存在不处理
	if !ok {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateAccept:
		// 修改状态，rtt，时间
		conn.state = connStateConnect
		conn.rtt = time.Now().Sub(conn.readTime)
		conn.readTime = time.Now()
		conn.lock.Unlock()
		// 添加到connected列表
		this.lock[serverConn].Lock()
		this.connected[serverConn][key] = conn
		this.lock[serverConn].Unlock()
		// 通知
		conn.connected <- connStateAccept
	default:
		// 其他不处理
		// connStateConnect，重复消息
		// connStateDial，服务端不可能
		// connStateClose，关闭了
		conn.lock.Unlock()
	}
	fmt.Fprintf(os.Stdout, "msg-connect: "+
		"token<%d>"+
		"\n",
		key.token,
	)
}

// 处理msgData
func (this *RUDP) handleMsgData(cs connCS, buf []byte, n int, addr *net.UDPAddr) {
	if n < msgDataPayload {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgConnectToken:])
	conn := this.connectedConn(cs, token, addr)
	if conn == nil {
		this.writeMsgInvalid(cs, token, buf, addr)
		return
	}
	// 添加数据，响应ack
	sn := binary.BigEndian.Uint32(buf[msgDataSN:])
	conn.readLock.Lock()
	if conn.readFromUDP(sn, buf[msgDataPayload:n]) {
		conn.writeMsgAck(buf, sn)
		// 可读通知
		select {
		case conn.readable <- 1:
		default:
		}
	}
	conn.readLock.Unlock()
	this.WriteToConn(buf[:msgAckLength], conn)
	fmt.Fprintf(os.Stdout, "msg-data: "+
		"token<%d>"+
		"sn<%d>"+
		"payload<%d>"+
		"\n",
		token,
		sn,
		n-msgDataPayload,
	)
}

// 处理msgAck
func (this *RUDP) handleMsgAck(cs connCS, buf []byte, n int, addr *net.UDPAddr) {
	if n != msgAckLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgConnectToken:])
	conn := this.connectedConn(cs, token, addr)
	if conn == nil {
		return
	}
	sn := binary.BigEndian.Uint32(buf[msgAckSN:])
	max_sn := binary.BigEndian.Uint32(buf[msgAckMaxSN:])
	remains := binary.BigEndian.Uint32(buf[msgAckRemains:])
	conn.writeLock.Lock()
	if sn < max_sn {
		conn.rmWriteDataBefore(max_sn, remains)
	} else {
		conn.rmWriteData(sn, remains)
	}
	conn.writeLock.Unlock()
	fmt.Fprintf(os.Stdout, "msg-ack: "+
		"token<%d>"+
		"sn<%d>"+
		"max sn<%d>"+
		"remains<%d>"+
		"\n",
		token,
		sn,
		max_sn,
		remains,
	)
}

// 处理msgInvalid
func (this *RUDP) handleMsgInvalid(cs connCS, buf []byte, n int, addr *net.UDPAddr) {
	if n != msgInvalidLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgInvalidToken:])
	conn := this.connectedConn(cs, token, addr)
	if conn == nil {
		return
	}
	this.freeConn(conn)
	fmt.Fprintf(os.Stdout, "msg-invalid: "+
		"token<%d>"+
		"\n",
		token,
	)
}

// 处理msgPing
func (this *RUDP) handleMsgPing(buf []byte, n int, addr *net.UDPAddr) {
	if n != msgPingLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgPingToken:])
	conn := this.connectedConn(serverConn, token, addr)
	if conn == nil {
		return
	}
	conn.writeMsgPong(buf)
	this.WriteToConn(buf[:msgPongLength], conn)
	fmt.Fprintf(os.Stdout, "msg-ping: "+
		"token<%d>"+
		"ping id<%d>"+
		"\n",
		token,
		binary.BigEndian.Uint32(buf[msgPongPingId:]),
	)
}

// 处理msgPong
func (this *RUDP) handleMsgPong(buf []byte, n int, addr *net.UDPAddr) {
	if n != msgPongLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgPingToken:])
	conn := this.connectedConn(clientConn, token, addr)
	if conn == nil {
		return
	}
	conn.readMsgPong(buf)
	fmt.Fprintf(os.Stdout, "msg-pong: "+
		"token<%d>"+
		"ping id<%d>"+
		"max sn<%d>"+
		"remins<%d>"+
		"\n",
		token,
		binary.BigEndian.Uint32(buf[msgPongPingId:]),
		binary.BigEndian.Uint32(buf[msgPongMaxSN:]),
		binary.BigEndian.Uint32(buf[msgPongRemains:]),
	)
}

// 发送msgInvalidC/msgInvalidS
func (this *RUDP) writeMsgInvalid(cs connCS, token uint32, buf []byte, addr *net.UDPAddr) {
	buf[msgType] = msgInvalid[cs]
	binary.BigEndian.PutUint32(buf[msgInvalidToken:], token)
	this.WriteTo(buf[:msgInvalidLength], addr)
}
