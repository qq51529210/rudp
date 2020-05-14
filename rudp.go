package rudp

import (
	"encoding/binary"
	"net"
	"runtime"
	"sync"
	"time"
)

type RUDP struct {
	conn         *net.UDPConn         // 底层socket
	wait         sync.WaitGroup       // 等待所有协程退出
	closeSignal  chan struct{}        // 通知所有协程退出的信号
	udpDataQueue chan *udpData        // 等待处理的原始的udp数据队列
	ioBytes      uint64RW             // udp读写的总字节
	connBuffer   uint32RW             // Conn读写缓存的默认大小
	lock         [2]sync.RWMutex      // 客户端/服务端的锁
	dialing      map[uint32]*Conn     // 客户端正在连接的Conn，key是token
	accepting    map[connKey]*Conn    // 服务端正在连接的Conn
	accepted     chan *Conn           // 服务端已经建立的连接，等待应用层处理
	connected    [2]map[connKey]*Conn // 已经建立连接的Conn
	token        [2]uint32            // 客户端/服务端，用于建立连接的随机数
	connectRTO   [2]time.Duration     // 客户端/服务端，建立连接的消息超时重发
}

// 使用默认值创建一个新的RUDP
func New(address string) (*RUDP, error) {
	var cfg Config
	cfg.Listen = address
	cfg.AcceptQueue = 128
	return NewWithConfig(&cfg)
}

// 使用配置值创建一个新的RUDP
func NewWithConfig(cfg *Config) (*RUDP, error) {
	addr, err := net.ResolveUDPAddr("udp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP(addr.Network(), addr)
	if err != nil {
		return nil, err
	}
	// 初始化
	p := new(RUDP)
	p.conn = conn
	p.closeSignal = make(chan struct{})
	p.udpDataQueue = make(chan *udpData, defaultInt(cfg.UDPDataQueue, defaultUDPDataQueue))
	p.dialing = make(map[uint32]*Conn)
	if cfg.AcceptQueue > 0 {
		p.accepted = make(chan *Conn, cfg.AcceptQueue)
	}
	p.accepting = make(map[connKey]*Conn)
	for i := 0; i < 2; i++ {
		p.connected[i] = make(map[connKey]*Conn)
		p.token[i] = _rand.Uint32()
	}
	p.connectRTO[ClientConn] = time.Duration(defaultInt(cfg.DialRTO, defaultConnectRTO)) * time.Millisecond
	p.connectRTO[ServerConn] = time.Duration(defaultInt(cfg.AcceptRTO, defaultConnectRTO)) * time.Millisecond
	p.connBuffer.r = uint32(defaultInt(cfg.ConnReadBuffer, defaultReadBuffer))
	p.connBuffer.w = uint32(defaultInt(cfg.ConnWriteBuffer, defaultWriteBuffer))
	// 启动读数据和处理数据协程
	go p.readUDPDataRoutine()
	for i := 0; i < defaultInt(cfg.UDPDataRoutine, runtime.NumCPU()); i++ {
		go p.handleUDPDataRoutine()
	}
	return p, nil
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

// 关闭RUDP，该RUDP对象，不可以再使用，net.Listener接口
func (this *RUDP) Close() error {
	// 关闭底层Conn
	err := this.conn.Close()
	if err != nil {
		return err
	}
	// 通知所有协程退出
	close(this.closeSignal)
	// 关闭所有的Conn
	for i := 0; i < 2; i++ {
		for _, v := range this.connected[i] {
			v.Close()
		}
	}
	for _, v := range this.dialing {
		v.Close()
	}
	for _, v := range this.accepting {
		v.Close()
	}
	// 等待所有协程退出
	this.wait.Wait()
	// 关闭udpData缓存chan
	close(this.udpDataQueue)
	return nil
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
	n, err := this.conn.WriteToUDP(data, conn.rIntAddr)
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

// net.Listener接口
func (this *RUDP) Accept() (net.Conn, error) {
	// 不作为服务端
	if this.accepted == nil {
		return nil, this.netOpError("listen", opErr("rudp is no a server"))
	}
	select {
	case conn, ok := <-this.accepted:
		// 等待新的连接
		if ok {
			return conn, nil
		}
	case <-this.closeSignal:
		// RUDP关闭信号
	}
	return nil, this.netOpError("listen", closeErr("rudp"))
}

// net.Listener接口
func (this *RUDP) Addr() net.Addr {
	return this.conn.LocalAddr()
}

// 连接指定的地址，返回
func (this *RUDP) Dial(addr string, timeout time.Duration) (*Conn, error) {
	conn, err := this.newDialConn(addr)
	if err != nil {
		return nil, err
	}
	// 初始化dial消息
	msg := udpDataPool.Get().(*udpData)
	conn.initMsgDial(msg)
	// 超时重发计时器
	ticker := time.NewTicker(this.connectRTO[ClientConn])
	// 回收资源
	defer func() {
		ticker.Stop()
		udpDataPool.Put(msg)
	}()
Loop:
	for {
		select {
		case now := <-ticker.C:
			// 超时
			if now.Sub(conn.time) >= timeout {
				err = conn.netOpError("dial", opErr("timeout"))
				break Loop
			}
			// 重发
			this.WriteTo(msg.buf[:msgDialLength], conn.rIntAddr)
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
			// RUDP关闭信号
			err = conn.netOpError("dial", closeErr("rudp"))
			break Loop
		}
	}
	// 出错
	close(conn.connectSignal)
	// 移除列表
	this.lock[ClientConn].Lock()
	delete(this.dialing, conn.cToken)
	this.lock[ClientConn].Unlock()
	// 返回
	return nil, err
}

// 创建一个新的Conn变量
func (this *RUDP) newConn(cs, state uint8, rAddr *net.UDPAddr) *Conn {
	p := new(Conn)
	p.cs = cs
	p.state = state
	p.lNatAddr = this.conn.LocalAddr().(*net.UDPAddr)
	p.rIntAddr = rAddr
	p.lMss = DetectMSS(rAddr)
	p.connectSignal = make(chan struct{})
	p.closeSignal = make(chan struct{})
	p.time = time.Now()
	p.rQue = readDataPool.Get().(*readData)
	p.wQue = writeDataPool.Get().(*writeData)
	return p
}

// 创建一个新的客户端Conn
func (this *RUDP) newDialConn(addr string) (*Conn, error) {
	// 解析地址
	rAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	// 新的Conn
	conn := this.newConn(ClientConn, connStateDial, rAddr)
	// 产生token
	this.lock[ClientConn].Lock()
	// 连接耗尽，2^32，理论上现有计算机不可能
	if uint32(len(this.dialing)) < maxUint32 {
		this.lock[ClientConn].Unlock()
		return nil, this.netOpError("dial", opErr("too many connections"))
	}
	for {
		conn.cToken = this.token[ClientConn]
		this.token[ClientConn]++
		// 检查是否存在，存在就递增
		_, ok := this.dialing[conn.cToken]
		if !ok {
			break
		}
	}
	// 添加到dialing列表
	this.dialing[conn.cToken] = conn
	this.lock[ClientConn].Unlock()
	return conn, nil
}

// 创建一个新的服务端Conn
func (this *RUDP) newAcceptConn(cToken uint32, cAddr *net.UDPAddr) *Conn {
	conn := this.newConn(ServerConn, connStateAccept, cAddr)
	conn.cToken = cToken
	this.lock[ServerConn].Lock()
	// 连接耗尽，2^32，理论上现有计算机不可能
	if uint32(len(this.dialing)) < maxUint32 {
		this.lock[ServerConn].Unlock()
		return nil
	}
	var key connKey
	key.cToken = conn.cToken
	for {
		key.sToken = this.token[ServerConn]
		this.token[ServerConn]++
		// 检查是否存在，存在就递增
		_, ok := this.accepting[key]
		if !ok {
			break
		}
	}
	conn.sToken = key.sToken
	this.accepting[key] = conn
	this.lock[ServerConn].Unlock()
	return conn
}

// 读udp数据的协程
func (this *RUDP) readUDPDataRoutine() {
	this.wait.Add(1)
	defer func() {
		this.Close()
		this.wait.Done()
	}()
	var err error
	for {
		msg := udpDataPool.Get().(*udpData)
		// 读取
		msg.len, msg.addr, err = this.conn.ReadFromUDP(msg.buf[:])
		if err != nil {
			// 出错，关闭
			return
		}
		this.ioBytes.r += uint64(msg.len)
		// 处理
		select {
		case this.udpDataQueue <- msg:
			// 添加到等待处理队列
		case <-this.closeSignal:
			// RUDP关闭信号
			return
		default:
			// 等待处理队列已满
			udpDataPool.Put(msg)
		}
	}
}

// 处理udp数据的协程
func (this *RUDP) handleUDPDataRoutine() {
	this.wait.Add(1)
	defer this.wait.Done()
	for {
		select {
		case msg := <-this.udpDataQueue:
			switch msg.buf[msgType] {
			case msgDial: // c->s，请求创建连接，握手1
				this.handleMsgDial(msg)
			case msgAccept: // s->c，接受连接，握手2
				this.handleMsgAccept(msg)
			case msgRefuse: // s->c，拒绝连接，握手2
				this.handleMsgRefuse(msg)
			case msgConnect: // c->s，收到接受连接的消息，握手3
				this.handleMsgConnect(msg)
			case msgDataC: // c->s，数据
				this.handleMsgDataC(msg)
			case msgDataS: // s->c，数据
				this.handleMsgDataS(msg)
			case msgAckC: // c->s，收到数据确认
				this.handleMsgAckC(msg)
			case msgAckS: // s->c，收到数据确认
				this.handleMsgAckS(msg)
			case msgCloseC: // c->s，关闭
				this.handleMsgCloseC(msg)
			case msgCloseS: // s->c，关闭
				this.handleMsgCloseS(msg)
			case msgPing: // c->s
				this.handleMsgPing(msg)
			case msgPong: // s->c
				this.handleMsgPong(msg)
			case msgInvalidC: // c<->s，无效的连接
				this.handleMsgInvalidC(msg)
			case msgInvalidS: // c<->s，无效的连接
				this.handleMsgInvalidS(msg)
			}
			udpDataPool.Put(msg)
		case <-this.closeSignal: // RUDP关闭信号
			return
		}
	}
}

// 服务端Conn写协程
func (this *RUDP) acceptConnRoutine(conn *Conn, timeout time.Duration) {
	this.wait.Add(1)
	// 初始化accept消息
	msg := udpDataPool.Get().(*udpData)
	conn.initMsgAccept(msg)
	// 超时计时器
	timer := time.NewTimer(0)
	defer func() {
		timer.Stop()
		this.closeConn(conn)
		this.wait.Done()
	}()
	// 发送msgAccept
WriteMsgAcceptLoop:
	for {
		select {
		case now := <-timer.C:
			// 超时
			if now.Sub(conn.time) >= timeout {
				return
			}
			// 重发
			this.WriteToConn(msg.buf[:msgAckLength], conn)
			// 重置计时器
			timer.Reset(this.connectRTO[ServerConn])
		case <-this.closeSignal:
			// RUDP关闭信号
			return
		case <-conn.connectSignal:
			// Conn连接通知
			break WriteMsgAcceptLoop
		case <-conn.closeSignal:
			// Conn关闭信号
			return
		}
	}
	this.connWriteLoop(conn, timer)
}

// 客户端Conn写协程
func (this *RUDP) connWriteRoutine(conn *Conn) {
	this.wait.Add(1)
	timer := time.NewTimer(conn.rto)
	defer func() {
		timer.Stop()
		this.closeConn(conn)
		this.wait.Done()
	}()
	this.connWriteLoop(conn, timer)
}

// Conn的写循环
func (this *RUDP) connWriteLoop(conn *Conn, timer *time.Timer) {
	for {
		select {
		case <-this.closeSignal:
			// RUDP关闭信号
			return
		case <-conn.closeSignal:
			// Conn关闭信号
			return
		case now := <-timer.C:
			// 超时重传检查
			conn.wLock.Lock()
			// 最大发送个数（对方能接受的数据量，不能接受，则单发一个）
			n := maxInt(1, int(conn.rRemains))
			// 没数据/超出最大发送个数
			p := conn.wQue.next
			for p != nil && n > 0 {
				// 距离上一次的发送，是否超时
				if now.Sub(p.last) >= conn.rto {
					this.WriteToConn(p.data.buf[:p.data.len], conn)
					// 记录发送时间
					p.last = now
					n--
				}
				p = p.next
			}
			conn.wLock.Unlock()
			// 重置计时器
			timer.Reset(conn.rto)
		}
	}
}

// 关闭并释放Conn的资源
func (this *RUDP) closeConn(conn *Conn) {

}

// 处理msgDial
func (this *RUDP) handleMsgDial(msg *udpData) {
	// 没有开启服务
	if this.accepted == nil {
		return
	}
	// 检查消息大小和版本
	if msg.len != msgDialLength || msg.buf[msgDialVersion] != msgVersion {
		return
	}
	// 客户端产生的token
	token := binary.BigEndian.Uint32(msg.buf[msgDialToken:])
	// 检查是否有accepting Conn
	var key connKey
	key.Init(msg.addr)
	this.lock[ServerConn].Lock()
	conn, ok := this.accepting[key]
	// 存在，这里不处理
	if ok {
		this.lock[ServerConn].Unlock()
		return
	}
	// 不存在，新的连接
	conn = this.newAcceptConn(token, msg.addr)
	// 创建失败
	if conn == nil {
		this.lock[ServerConn].Unlock()
		// 发送refuse消息
		msg.buf[msgType] = msgRefuse
		binary.BigEndian.PutUint32(msg.buf[msgRefuseToken:], token)
		this.WriteTo(msg.buf[:msgRefuseLength], msg.addr)
		return
	}
	// 添加到accepting列表
	this.accepting[key] = conn
	this.lock[ServerConn].Unlock()
	// 对方的监听地址
	conn.rNatAddr = new(net.UDPAddr)
	conn.rNatAddr.IP = append(conn.lIntAddr.IP, msg.buf[msgDialLocalIP:msgDialLocalPort]...)
	conn.rNatAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgDialLocalPort:]))
	// 本地的公网地址
	conn.lIntAddr = new(net.UDPAddr)
	conn.lIntAddr.IP = append(conn.lIntAddr.IP, msg.buf[msgDialRemoteIP:msgDialRemotePort]...)
	conn.lIntAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgDialRemotePort:]))
	// 对方的mss
	conn.rMss = binary.BigEndian.Uint16(msg.buf[msgDialMSS:])
	// 对方的读写缓存
	conn.rMaxBuff.r = binary.BigEndian.Uint32(msg.buf[msgDialReadBuffer:])
	conn.rMaxBuff.w = binary.BigEndian.Uint32(msg.buf[msgDialWriteBuffer:])
	// 启动服务端Conn写协程
	go this.acceptConnRoutine(conn, time.Duration(binary.BigEndian.Uint64(msg.buf[msgDialTimeout:])))
}

// 处理msgAccept
func (this *RUDP) handleMsgAccept(msg *udpData) {
	// 检查消息大小和版本
	if msg.len != msgAcceptLength || msg.buf[msgAcceptVersion] != msgVersion {
		return
	}
	// c/s token
	cToken := binary.BigEndian.Uint32(msg.buf[msgAcceptCToken:])
	sToken := binary.BigEndian.Uint32(msg.buf[msgAcceptSToken:])
	// 检查 dialing Conn
	this.lock[ClientConn].RLock()
	conn, ok := this.dialing[cToken]
	this.lock[ClientConn].RUnlock()
	if !ok {
		// 发送msgInvalidC，通知对方不要在发送msgAccept了
		this.writeMsgInvalid(msg, msgInvalidC, cToken, sToken)
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateDial:
		conn.sToken = sToken
		conn.state = connStateConnect
		conn.lock.Unlock()
		// 添加到已连接列表
		this.lock[ClientConn].Lock()
		var key connKey
		key.Init(conn.rIntAddr)
		key.cToken = cToken
		key.sToken = sToken
		this.connected[ClientConn][key] = conn
		this.lock[ClientConn].Unlock()
		// 对方的监听地址
		conn.rNatAddr = new(net.UDPAddr)
		conn.rNatAddr.IP = append(conn.lIntAddr.IP, msg.buf[msgAcceptLocalIP:msgAcceptLocalPort]...)
		conn.rNatAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgAcceptLocalPort:]))
		// 本地的公网地址
		conn.lIntAddr = new(net.UDPAddr)
		conn.lIntAddr.IP = append(conn.lIntAddr.IP, msg.buf[msgAcceptRemoteIP:msgAcceptRemotePort]...)
		conn.lIntAddr.Port = int(binary.BigEndian.Uint16(msg.buf[msgAcceptRemotePort:]))
		// 对方的mss
		conn.rMss = binary.BigEndian.Uint16(msg.buf[msgAcceptMSS:])
		// 对方的读写缓存
		conn.rMaxBuff.r = binary.BigEndian.Uint32(msg.buf[msgAcceptReadBuffer:])
		conn.rMaxBuff.w = binary.BigEndian.Uint32(msg.buf[msgAcceptWriteBuffer:])
		// 通知连接
		close(conn.connectSignal)
		// 启动客户端Conn写协程
		go this.connWriteRoutine(conn)
	case connStateConnect:
		conn.lock.Unlock()
	default:
		// 其他状态不处理
		conn.lock.Unlock()
		return
	}
	// 发送connect消息
	this.writeMsgConnect(conn, msg)
}

// 处理msgRefuse
func (this *RUDP) handleMsgRefuse(msg *udpData) {
	// 检查消息大小和版本
	if msg.n != msgRefuseLength || msg.b[msgRefuseVersion] != msgVersion {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgRefuseToken:])
	// dialing Conn
	this.client.RLock()
	conn, ok := this.client.dialing[token]
	this.client.RUnlock()
	// 不存在不处理
	if !ok {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateDial:
		// 关闭
		conn.state = connStateClose
		conn.lock.Unlock()
		// 通知
		close(conn.connectSignal)
	default:
		// 其他状态不处理
		// connStateAccept: 客户端不可能
		// connStateClose: 正在关闭
		// connStateConnect: 已经连接
		conn.lock.Unlock()
	}
}

// 处理msgConnect
func (this *RUDP) handleMsgConnect(msg *udpData) {
	// 没有开启服务
	if this.server.accepted == nil {
		return
	}
	// 检查消息大小
	if msg.n != msgConnectLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgConnectToken:])
	// connected Conn
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	// 不存在
	if !ok {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateAccept:
		// 第一次收到msgConnect
		conn.state = connStateConnect
		conn.lock.Unlock()
		// 通知
		close(conn.connectSignal)
	default:
		// 其他不处理
		// connStateConnect，重复消息
		// connStateDial，服务端不可能
		// connStateClose，关闭了
		conn.lock.Unlock()
	}
}

// 处理msgDataC
func (this *RUDP) handleMsgDataC(msg *udpData) {
	// 没有开启服务
	if this.server.accepted == nil {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgDataToken:])
	// server connected Conn
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	// 不存在
	if !ok {
		this.writeMsgInvalid(msg, msgInvalidS, token)
		return
	}
	this.handleMsgData(conn, msg)
}

// 处理msgDataS
func (this *RUDP) handleMsgDataS(msg *udpData) {
	token := binary.BigEndian.Uint32(msg.b[msgDataToken:])
	// client connected Conn
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	// 不存在
	if !ok {
		this.writeMsgInvalid(msg, msgInvalidC, token)
		return
	}
	this.handleMsgData(conn, msg)
}

// 处理msgData
func (this *RUDP) handleMsgData(conn *Conn, msg *udpData) {
	// 数据块序号
	sn := binary.BigEndian.Uint32(msg.b[msgDataSN:])
	conn.rBuf.Lock()
	conn.rBuf.AddData(sn, msg.b[msgDataPayload:msg.n])
	// 初始化ack消息
	conn.ackMsg(sn, msg)
	conn.rBuf.Unlock()
	this.writeMsgConnect(conn, msg)
}

// 处理msgAckC
func (this *RUDP) handleMsgAckC(msg *udpData) {
	// 没有开启服务
	if this.server.accepted == nil {
		return
	}
	if msg.n != msgAckLength {
		return
	}
	this.client.RLock()
	conn, ok := this.client.connected[binary.BigEndian.Uint32(msg.b[msgAckToken:])]
	this.client.RUnlock()
	if !ok {
		_dataPool.Put(msg)
		return
	}
	this.handleMsgAck(conn, msg)
}

// 处理msgAckS
func (this *RUDP) handleMsgAckS(msg *udpData) {
	if msg.n != msgAckLength {
		return
	}
	this.server.RLock()
	conn, ok := this.server.connected[binary.BigEndian.Uint32(msg.b[msgAckToken:])]
	this.server.RUnlock()
	if !ok {
		_dataPool.Put(msg)
		return
	}
	this.handleMsgAck(conn, msg)
}

// 处理msgAck
func (this *RUDP) handleMsgAck(conn *Conn, msg *udpData) {
	conn.wBuf.Lock()
	ok := conn.wBuf.Remove(msg)
	conn.wBuf.Unlock()
	// 通知可写
	if ok {
		select {
		case conn.wBuf.enable <- 1:
		default:
		}
	}
}

// 处理msgCloseC
func (this *RUDP) handleMsgCloseC(msg *udpData) {
	// 没有开启服务
	if this.server.accepted == nil {
		return
	}
	if msg.n != msgCloseLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgCloseToken:])
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	if ok {
		conn.Close()
		this.writeMsgClose(msg, msgClose[conn.cs], token)
	} else {
		this.writeMsgInvalid(msg, msgInvalidS, token)
	}
}

// 处理msgCloseS
func (this *RUDP) handleMsgCloseS(msg *udpData) {
	if msg.n != msgCloseLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgCloseToken:])
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	if ok {
		conn.Close()
		this.writeMsgClose(msg, msgClose[conn.cs], token)
	} else {
		this.writeMsgInvalid(msg, msgInvalidS, token)
	}
}

// 处理msgPing
func (this *RUDP) handleMsgPing(msg *udpData) {
	if msg.n != msgPingLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgPingToken:])
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	if !ok {
		return
	}
	conn.pongMsg(msg, binary.BigEndian.Uint32(msg.b[msgPingId:]))
	n, err := this.WriteTo(msg.b[:msgCloseLength], msg.a)
	if err == nil {
		this.totalBytes.w += uint64(n)
	}
}

// 处理msgPong
func (this *RUDP) handleMsgPong(msg *udpData) {
	if msg.n != msgPongLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgPongToken:])
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	if !ok {
		return
	}
	id := binary.BigEndian.Uint32(msg.b[msgPongSN:])
	conn.lock.Lock()
	if id != conn.pingId {
		conn.lock.Unlock()
		return
	}
	conn.pingId++
	left := binary.BigEndian.Uint32(msg.b[msgPongBuffer:])
	conn.wBuf.canWrite = left
	conn.lock.Unlock()
}

// 处理msgInvalidC
func (this *RUDP) handleMsgInvalidC(msg *udpData) {
	if msg.n != msgInvalidLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgInvalidToken:])
	this.server.RLock()
	conn, ok := this.server.connected[token]
	this.server.RUnlock()
	if ok {
		conn.Close()
	}
}

// 处理msgInvalidS
func (this *RUDP) handleMsgInvalidS(msg *udpData) {
	if msg.n != msgInvalidLength {
		return
	}
	token := binary.BigEndian.Uint32(msg.b[msgInvalidToken:])
	this.client.RLock()
	conn, ok := this.client.connected[token]
	this.client.RUnlock()
	if ok {
		conn.Close()
	}
}

// 发送msgInvalidC/msgInvalidS
func (this *RUDP) writeMsgInvalid(msg *udpData, _type byte, cToken, sToken uint32) {
	msg.buf[msgType] = _type
	binary.BigEndian.PutUint32(msg.buf[msgInvalidCToken:], cToken)
	binary.BigEndian.PutUint32(msg.buf[msgInvalidSToken:], sToken)
}
