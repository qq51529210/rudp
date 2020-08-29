package rudp

import (
	"encoding/binary"
	"log"
	"net"
	"runtime"
	"sync"
	"time"
)

func init() {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
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
	p.conn = conn
	p.quit = make(chan struct{})
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
	conn       *net.UDPConn         // 底层socket
	wait       sync.WaitGroup       // 等待所有协程退出
	quit       chan struct{}        // 通知所有协程退出的信号
	closed     bool                 // 是否调用了Close()
	ioBytes    uint64RW             // udp读写的总字节
	accepted   chan *Conn           // 服务端已经建立，等待应用层处理的连接
	lock       [2]sync.RWMutex      // 客户端/服务端，锁
	token      [2]uint32            // 客户端/服务端，建立连接起始随机数
	connecting [2]map[connKey]*Conn // 客户端/服务端，正在建立连接的Conn
	connected  [2]map[connKey]*Conn // 客户端/服务端，已经建立连接的Conn
	connBuffer uint32RW             // Conn读写缓存的默认大小
	connRTO    time.Duration        // Conn建立连接的消息超时重发
	connMinRTO time.Duration        // Conn最小rto
	connMaxRTO time.Duration        // Conn最大rto
}

// 返回net.OpError对象
func (r *RUDP) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: nil,
		Addr:   r.conn.LocalAddr(),
		Err:    err,
	}
}

// net.Listener接口
func (r *RUDP) Close() error {
	// 关闭底层Conn
	err := r.conn.Close()
	if err != nil {
		return err
	}
	r.closed = true
	// 通知所有协程退出
	close(r.quit)
	close(r.accepted)
	// 关闭所有的Conn
	for i := 0; i < 2; i++ {
		r.lock[i].Lock()
		for _, v := range r.connecting[i] {
			v.Close()
		}
		for _, v := range r.connected[i] {
			v.Close()
		}
		r.lock[i].Unlock()
	}
	// 等待所有协程退出
	r.wait.Wait()
	return nil
}

// net.Listener接口
func (r *RUDP) Accept() (net.Conn, error) {
	// 不作为服务端
	if r.accepted == nil {
		return nil, r.netOpError("listen", opErr("rudp is no a server"))
	}
	select {
	case conn, ok := <-r.accepted: // 等待新的连接
		if ok {
			return conn, nil
		}
	case <-r.quit: // RUDP关闭信号
	}
	return nil, r.netOpError("accept", closeErr("rudp"))
}

// net.Listener接口
func (r *RUDP) Addr() net.Addr {
	return r.conn.LocalAddr()
}

// 使用net.UDPConn向指定地址发送指定数据
func (r *RUDP) WriteTo(data []byte, addr *net.UDPAddr) (int, error) {
	n, err := r.conn.WriteToUDP(data, addr)
	if err == nil {
		r.ioBytes.w += uint64(n)
	}
	return n, err
}

// 使用net.UDPConn向指定地址发送指定数据
func (r *RUDP) WriteToConn(data []byte, conn *Conn) (int, error) {
	n, err := r.conn.WriteToUDP(data, conn.rIAddr)
	if err == nil {
		conn.ioBytes.w += uint64(n)
		r.ioBytes.w += uint64(n)
	}
	return n, err
}

// 返回net.UDPConn的本地地址
func (r *RUDP) LocalAddr() net.Addr {
	return r.conn.LocalAddr()
}

// 读取的总字节，不包括ip和udp头
func (r *RUDP) ReadBytes() uint64 {
	return r.ioBytes.r
}

// 发送的总字节，不包括ip和udp头
func (r *RUDP) WriteBytes() uint64 {
	return r.ioBytes.w
}

// 读udp数据的协程
func (r *RUDP) handleUDPDataRoutine() {
	r.wait.Add(1)
	defer r.wait.Done()
	var err error
	var n int
	var buf udpBuf
	var addr *net.UDPAddr
	for !r.closed {
		// 读取数据并处理
		n, addr, err = r.conn.ReadFromUDP(buf[:])
		if err != nil {
			// todo: 日志
			continue
		}
		r.ioBytes.r += uint64(n)
		// 处理
		switch buf[msgType] {
		case msgDial:
			r.handleMsgDial(buf[:], n, addr)
		case msgAccept:
			r.handleMsgAccept(buf[:], n, addr)
		case msgRefuse:
			r.handleMsgRefuse(buf[:], n, addr)
		case msgConnect:
			r.handleMsgConnect(buf[:], n, addr)
		case msgDataC:
			r.handleMsgData(connServer, buf[:], n, addr)
		case msgDataS:
			r.handleMsgData(connClient, buf[:], n, addr)
		case msgAckC:
			r.handleMsgAck(connServer, buf[:], n, addr)
		case msgAckS:
			r.handleMsgAck(connClient, buf[:], n, addr)
		case msgPing:
			r.handleMsgPing(buf[:], n, addr)
		case msgPong:
			r.handleMsgPong(buf[:], n, addr)
		case msgInvalidC:
			r.handleMsgInvalid(connServer, buf[:], n, addr)
		case msgInvalidS:
			r.handleMsgInvalid(connClient, buf[:], n, addr)
		}
	}
}

// 连接指定的地址
func (r *RUDP) Dial(addr string, timeout time.Duration) (*Conn, error) {
	// 新的客户端Conn
	conn, err := r.newDialConn(addr)
	if err != nil {
		return nil, err
	}
	// 发送msgDial
	var buf [maxMSS]byte
	conn.writeMsgDial(buf[:], timeout)
	for err == nil {
		select {
		case now := <-conn.timer.C: // 超时重发
			if now.Sub(conn.readTime) >= timeout {
				err = conn.netOpError("dial", opErr("timeout"))
			} else {
				r.WriteToConn(buf[:msgDialLength], conn)
				conn.timer.Reset(conn.rto)
			}
		case state := <-conn.connected: // 建立连接结果通知
			switch state {
			case connStateConnect: // 已经建立连接
				return conn, nil
			case connStateDial: // 服务端拒绝连接
				err = conn.netOpError("dial", opErr("connect refuse"))
			default: // 其他状态都是逻辑bug
				err = conn.netOpError("dial", opErr("code bug"))
			}
		case <-r.quit: // RUDP被关闭
			err = conn.netOpError("dial", closeErr("rudp"))
		}
	}
	r.freeConn(conn)
	return nil, err
}

// 创建一个新的Conn变量
func (r *RUDP) newConn(cs connCS, state connState, rAddr *net.UDPAddr) *Conn {
	p := new(Conn)
	p.cs = cs
	p.state = state
	p.connected = make(chan connState, 1)
	p.closed = make(chan struct{})
	p.timer = time.NewTimer(0)
	p.lLAddr = r.conn.LocalAddr().(*net.UDPAddr)
	p.lIAddr = new(net.UDPAddr)
	p.rLAddr = new(net.UDPAddr)
	p.rIAddr = rAddr
	p.readTime = time.Now()
	p.readQueue = readDataPool.Get().(*readData)
	p.readQueue.next = nil
	p.readable = make(chan int, 1)
	p.readable <- 1
	p.writeQueueHead = writeDataPool.Get().(*writeData)
	p.writeQueueHead.next = nil
	p.writeQueueTail = p.writeQueueHead
	p.writeable = make(chan int, 1)
	p.writeable <- 1
	p.rto = r.connRTO
	p.minRTO = r.connMinRTO
	p.maxRTO = r.connMaxRTO
	return p
}

// 创建一个新的客户端Conn
func (r *RUDP) newDialConn(addr string) (*Conn, error) {
	rAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	var key connKey
	key.Init(rAddr)
	r.lock[connClient].Lock()
	// 连接耗尽
	if len(r.connecting[connClient]) >= maxConn {
		r.lock[connClient].Unlock()
		return nil, r.netOpError("dial", opErr("too many connections"))
	}
	conn := r.newConn(connClient, connStateDial, rAddr)
	// 检查一个没有使用的token
	for !r.closed {
		key.token = r.token[connClient]
		r.token[connClient]++
		_, ok := r.connecting[connClient][key]
		if !ok {
			conn.cToken = key.token
			r.connecting[connClient][key] = conn
			break
		}
	}
	r.lock[connClient].Unlock()
	// 探测mss
	conn.writeMSS = DetectMSS(rAddr)
	// 确定发送队列最大长度
	conn.writeQueueMaxLen = calcMaxLen(r.connBuffer.w, conn.writeMSS)
	return conn, nil
}

// 创建一个新的服务端Conn
func (r *RUDP) newAcceptConn(token uint32, cAddr *net.UDPAddr) *Conn {
	var key connKey
	key.token = token
	key.Init(cAddr)
	// 检查accepting Conn
	r.lock[connServer].Lock()
	conn, ok := r.connecting[connServer][key]
	// 存在
	if ok {
		r.lock[connServer].Unlock()
		return conn
	}
	// 连接耗尽
	if len(r.connecting[connServer]) >= maxConn {
		r.lock[connServer].Unlock()
		return nil
	}
	conn = r.newConn(connServer, connStateAccept, cAddr)
	conn.cToken = token
	r.connecting[connServer][key] = conn
	// 检查一个没有使用的token
	for !r.closed {
		key.token = r.token[connServer]
		r.token[connServer]++
		_, ok := r.connected[connServer][key]
		if !ok {
			conn.sToken = key.token
			r.connected[connServer][key] = conn
			break
		}
	}
	r.lock[connServer].Unlock()
	// 探测mss
	conn.writeMSS = DetectMSS(cAddr)
	// 确定发送队列最大长度
	conn.writeQueueMaxLen = calcMaxLen(r.connBuffer.w, conn.writeMSS)
	return conn
}

// 关闭并释放Conn的资源
func (r *RUDP) freeConn(conn *Conn) {
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
	r.lock[conn.cs].Lock()
	delete(r.connecting[conn.cs], key)
	// 移除connected
	key.token = conn.sToken
	delete(r.connected[conn.cs], key)
	r.lock[conn.cs].Unlock()
	// 释放资源
	close(conn.closed)
	close(conn.connected)
	close(conn.readable)
	close(conn.writeable)
	conn.timer.Stop()
	q := conn.readQueue
	for q != nil {
		n := q.next
		readDataPool.Put(r)
		q = n
	}
	w := conn.writeQueueHead
	for w != nil {
		n := w.next
		writeDataPool.Put(w)
		w = n
	}
}

// 客户端Conn写协程
func (r *RUDP) connClientRoutine(conn *Conn) {
	r.wait.Add(1)
	defer func() {
		r.freeConn(conn)
		r.wait.Done()
	}()
	r.connWriteLoop(conn)
}

// 服务端Conn写协程
func (r *RUDP) connServerRoutine(conn *Conn, timeout time.Duration) {
	r.wait.Add(1)
	defer func() {
		r.freeConn(conn)
		r.wait.Done()
	}()
	// 发送msgAccept
	var buf udpBuf
	conn.writeMsgAccept(buf[:])
	for {
		select {
		case <-r.quit: // RUDP关闭信号
			return
		case <-conn.closed: // Conn关闭信号
			return
		case now := <-conn.timer.C: // 超时重发
			if now.Sub(conn.readTime) >= timeout {
				return
			}
			r.WriteToConn(buf[:msgAcceptLength], conn)
			conn.timer.Reset(conn.rto)
		case <-conn.connected: // Conn连接通知
			r.accepted <- conn
			r.connWriteLoop(conn)
			return
		}
	}
}

// 获取connecting Conn
func (r *RUDP) connectingConn(cs uint8, token uint32, addr *net.UDPAddr) *Conn {
	var key connKey
	key.Init(addr)
	key.token = token
	r.lock[cs].RLock()
	conn, _ := r.connecting[cs][key]
	r.lock[cs].RUnlock()
	return conn
}

// 获取connected Conn
func (r *RUDP) connectedConn(cs connCS, token uint32, addr *net.UDPAddr) *Conn {
	var key connKey
	key.Init(addr)
	key.token = token
	r.lock[cs].RLock()
	conn, _ := r.connected[cs][key]
	r.lock[cs].RUnlock()
	return conn
}

// 客户端Conn写协程
func (r *RUDP) connWriteLoop(conn *Conn) {
	for {
		select {
		case <-r.quit: // RUDP关闭信号
			return
		case <-conn.closed: // Conn关闭信号
			return
		case now := <-conn.timer.C: // 超时重传检查
			conn.writeLock.Lock()
			conn.writeToUDP(func(data []byte) {
				r.WriteToConn(data, conn)
			}, now)
			conn.writeLock.Unlock()
			conn.timer.Reset(conn.rto)
		}
	}
}

// 处理msgDial
func (r *RUDP) handleMsgDial(buf []byte, n int, addr *net.UDPAddr) {
	// 消息大小和版本
	if n != msgDialLength || binary.BigEndian.Uint32(buf[msgDialVersion:]) != msgVersion {
		return
	}
	// 客户端token
	cToken := binary.BigEndian.Uint32(buf[msgToken:])
	// 没有开启服务，发送msgRefuse
	if r.accepted == nil {
		buf[msgType] = msgRefuse
		binary.BigEndian.PutUint32(buf[msgRefuseToken:], cToken)
		r.WriteTo(buf[:msgRefuseLength], addr)
		return
	}
	conn := r.newAcceptConn(cToken, addr)
	if conn == nil {
		// 响应msgRefuse
		buf[msgType] = msgRefuse
		binary.BigEndian.PutUint32(buf[msgRefuseToken:], cToken)
		r.WriteTo(buf[:msgRefuseLength], addr)
		return
	}
	// 读取msgDial的字段
	conn.readMsgDial(buf, r.connBuffer.r)
	// 启动服务端Conn写协程
	go r.connServerRoutine(conn, time.Duration(binary.BigEndian.Uint64(buf[msgDialTimeout:])))
	//log.Printf("<%v>msg-dial: "+
	//	"token<%d>"+
	//	"version<%d>"+
	//	"remote listen address<%s>"+
	//	"local internet address<%s>"+
	//	"mss<%d>"+
	//	"timeout<%v>"+
	//	"\n",
	//	conn.cs,
	//	conn.cToken,
	//	binary.BigEndian.Uint32(buf[msgDialVersion:]),
	//	conn.rLAddr.String(),
	//	conn.lIAddr.String(),
	//	conn.readMSS,
	//	time.Duration(binary.BigEndian.Uint64(buf[msgDialTimeout:])),
	//)
}

// 处理msgAccept
func (r *RUDP) handleMsgAccept(buf []byte, n int, addr *net.UDPAddr) {
	// 检查消息大小和版本
	if n != msgAcceptLength || binary.BigEndian.Uint32(buf[msgAcceptVersion:]) != msgVersion {
		return
	}
	// connecting Conn
	var key connKey
	key.Init(addr)
	key.token = binary.BigEndian.Uint32(buf[msgAcceptCToken:])
	r.lock[connClient].RLock()
	conn, ok := r.connecting[connClient][key]
	r.lock[connClient].RUnlock()
	if !ok {
		// 发送msgInvalid，通知对方不要在发送msgAccept了
		r.writeMsgInvalid(connClient, binary.BigEndian.Uint32(buf[msgAcceptSToken:]), buf, addr)
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateDial:
		// 保存sToken，修改状态，计算rto
		conn.sToken = binary.BigEndian.Uint32(buf[msgAcceptSToken:])
		conn.state = connStateConnect
		conn.rtt = time.Now().Sub(conn.readTime)
		conn.rttVar = conn.rtt
		conn.lock.Unlock()
		// 添加到connected列表
		key.token = conn.sToken
		r.lock[connClient].Lock()
		r.connected[connClient][key] = conn
		r.lock[connClient].Unlock()
		// 读取msgAccept字段
		conn.readMsgAccept(buf, r.connBuffer.r)
		// 通知连接
		conn.connected <- connStateConnect
		// 启动客户端Conn写协程
		go r.connClientRoutine(conn)
	case connStateConnect:
		// 已经是连接状态，说明是重复的消息
		conn.lock.Unlock()
	default:
		// connStateClose
		conn.lock.Unlock()
		// 发送msgInvalid，通知对方不要在发送msgAccept了
		r.writeMsgInvalid(connClient, binary.BigEndian.Uint32(buf[msgAcceptSToken:]), buf, addr)
		return
	}
	// 发送msgConnect
	conn.writeMsgConnect(buf)
	r.WriteToConn(buf[:msgConnectLength], conn)
	//log.Printf("<%v>msg-accept: "+
	//	"ctoken<%d>"+
	//	"stoken<%d>"+
	//	"version<%d>"+
	//	"remote listen address<%s>"+
	//	"local internet address<%s>"+
	//	"mss<%d>"+
	//	"\n",
	//	conn.cs,
	//	conn.cToken,
	//	conn.sToken,
	//	binary.BigEndian.Uint32(buf[msgAcceptVersion:]),
	//	conn.rLAddr.String(),
	//	conn.lIAddr.String(),
	//	conn.readMSS,
	//)
}

// 处理msgRefuse
func (r *RUDP) handleMsgRefuse(buf []byte, n int, addr *net.UDPAddr) {
	// 检查消息大小和版本
	if n != msgRefuseLength || binary.BigEndian.Uint32(buf[msgRefuseVersion:]) != msgVersion {
		return
	}
	// connecting Conn
	var key connKey
	key.Init(addr)
	key.token = binary.BigEndian.Uint32(buf[msgRefuseToken:])
	r.lock[connClient].RLock()
	conn, ok := r.connecting[connClient][key]
	r.lock[connClient].RUnlock()
	// 不存在不处理
	if !ok {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateDial:
		conn.connected <- connStateDial
	default: // 其他状态不处理
	}
	conn.lock.Unlock()
	log.Printf("<%v>msg-refuse: "+
		"token<%d>"+
		"version<%d>"+
		"\n",
		conn.cs,
		key.token,
		binary.BigEndian.Uint32(buf[msgRefuseVersion:]),
	)
}

// 处理msgConnect
func (r *RUDP) handleMsgConnect(buf []byte, n int, addr *net.UDPAddr) {
	// 检查消息大小
	if n != msgConnectLength {
		return
	}
	// 没有开启服务
	if r.accepted == nil {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgConnectToken:])
	conn := r.connectedConn(connServer, token, addr)
	if conn == nil {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateAccept:
		// 修改状态，rtt，时间
		conn.state = connStateConnect
		conn.rtt = time.Now().Sub(conn.readTime)
		conn.rttVar = conn.rtt
		// 通知
		conn.connected <- connStateConnect
	default: // 其他不处理
	}
	conn.lock.Unlock()
	//log.Printf("<%v>msg-connect: "+
	//	"token<%d>"+
	//	"\n",
	//	conn.cs,
	//	token,
	//)
}

// 处理msgData
func (r *RUDP) handleMsgData(cs connCS, buf []byte, n int, addr *net.UDPAddr) {
	if n < msgDataPayload {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgDataToken:])
	conn := r.connectedConn(cs, token, addr)
	if conn == nil {
		r.writeMsgInvalid(cs, token, buf, addr)
		return
	}
	// 添加数据
	sn := binary.BigEndian.Uint32(buf[msgDataSN:])
	conn.readLock.Lock()
	ok := conn.readFromUDP(sn, buf[msgDataPayload:n])
	if ok {
		conn.writeMsgAck(buf, sn)
		conn.readLock.Unlock()
		// 可读通知
		select {
		case conn.readable <- 1:
		default:
		}
		// 响应ack
		r.WriteToConn(buf[:msgAckLength], conn)
		//log.Printf("<%v>msg-data: "+
		//	"token<%d>"+
		//	"sn<%d>"+
		//	"payload<%d>"+
		//	"\n",
		//	conn.cs,
		//	token,
		//	sn,
		//	n-msgDataPayload,
		//)
	} else {
		conn.readLock.Unlock()
	}
}

// 处理msgAck
func (r *RUDP) handleMsgAck(cs connCS, buf []byte, n int, addr *net.UDPAddr) {
	if n != msgAckLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgConnectToken:])
	conn := r.connectedConn(cs, token, addr)
	if conn == nil {
		return
	}
	sn := binary.BigEndian.Uint32(buf[msgAckSN:])
	maxSn := binary.BigEndian.Uint32(buf[msgAckMaxSN:])
	remains := binary.BigEndian.Uint32(buf[msgAckRemains:])
	//ack_id := binary.BigEndian.Uint32(buf[msgAckId:])
	var ok bool
	conn.writeLock.Lock()
	if sn < maxSn {
		ok = conn.removeWriteDataBefore(maxSn, remains)
	} else {
		ok = conn.removeWriteData(sn, remains)
	}
	conn.writeLock.Unlock()
	if ok {
		// 可写通知
		select {
		case conn.writeable <- 1:
		default:
		}
		//log.Printf("<%v>msg-ack: "+
		//	"token<%d>"+
		//	"sn<%d>"+
		//	"max sn<%d>"+
		//	"remains<%d>"+
		//	"id<%d>"+
		//	"\n",
		//	conn.cs,
		//	token,
		//	sn,
		//	max_sn,
		//	remains,
		//	ack_id,
		//)
	}
}

// 处理msgInvalid
func (r *RUDP) handleMsgInvalid(cs connCS, buf []byte, n int, addr *net.UDPAddr) {
	if n != msgInvalidLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgInvalidToken:])
	conn := r.connectedConn(cs, token, addr)
	if conn == nil {
		return
	}
	conn.Close()
	log.Printf("<%v>msg-invalid: "+
		"token<%d>"+
		"\n",
		conn.cs,
		token,
	)
}

// 处理msgPing
func (r *RUDP) handleMsgPing(buf []byte, n int, addr *net.UDPAddr) {
	if n != msgPingLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgPingToken:])
	conn := r.connectedConn(connServer, token, addr)
	if conn == nil {
		return
	}
	// 发送msgPong
	conn.writeMsgPong(buf)
	r.WriteToConn(buf[:msgPongLength], conn)
	log.Printf("<%v>msg-ping: "+
		"token<%d>"+
		"ping id<%d>"+
		"\n",
		conn.cs,
		token,
		binary.BigEndian.Uint32(buf[msgPongPingId:]),
	)
}

// 处理msgPong
func (r *RUDP) handleMsgPong(buf []byte, n int, addr *net.UDPAddr) {
	if n != msgPongLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(buf[msgPingToken:])
	conn := r.connectedConn(connClient, token, addr)
	if conn == nil {
		return
	}
	// 读取msgPong字段
	if conn.readMsgPong(buf) {
		// 可写通知
		select {
		case conn.writeable <- 1:
		default:
		}
	}
	log.Printf("<%v>msg-pong: "+
		"token<%d>"+
		"ping id<%d>"+
		"max sn<%d>"+
		"remins<%d>"+
		"\n",
		conn.cs,
		token,
		binary.BigEndian.Uint32(buf[msgPongPingId:]),
		binary.BigEndian.Uint32(buf[msgPongMaxSN:]),
		binary.BigEndian.Uint32(buf[msgPongRemains:]),
	)
}

// 发送msgInvalid
func (r *RUDP) writeMsgInvalid(cs connCS, token uint32, buf []byte, addr *net.UDPAddr) {
	buf[msgType] = msgInvalid[cs]
	binary.BigEndian.PutUint32(buf[msgInvalidToken:], token)
	r.WriteTo(buf[:msgInvalidLength], addr)
}
