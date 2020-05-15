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
	accepted     chan *Conn           // 服务端已经建立，等待应用层处理的连接
	connected    [2]map[connKey]*Conn // 已经建立连接的Conn
	token        [2]uint32            // 客户端/服务端，用于建立连接的随机数
	connectRTO   [2]time.Duration     // 客户端/服务端，建立连接的消息超时重发
}

// 使用默认值创建一个具备cs能力的RUDP
func New(address string) (*RUDP, error) {
	var cfg Config
	cfg.Listen = address
	cfg.AcceptQueue = 128
	return NewWithConfig(&cfg)
}

// 使用配置值创建一个新的RUDP，server能力需要配置Config.AcceptQueue
func NewWithConfig(cfg *Config) (*RUDP, error) {
	addr, err := net.ResolveUDPAddr("udp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP(addr.Network(), addr)
	if err != nil {
		return nil, err
	}
	// 初始化成员变量
	p := new(RUDP)
	p.conn = conn
	p.closeSignal = make(chan struct{})
	p.udpDataQueue = make(chan *udpData, defaultInt(cfg.UDPDataQueue, defaultUDPDataQueue))
	p.dialing = make(map[uint32]*Conn)
	p.accepting = make(map[connKey]*Conn)
	if cfg.AcceptQueue > 0 {
		p.accepted = make(chan *Conn, cfg.AcceptQueue)
	}
	for i := 0; i < 2; i++ {
		p.connected[i] = make(map[connKey]*Conn)
		p.token[i] = _rand.Uint32()
	}
	p.connectRTO[ClientConn] = time.Duration(defaultInt(cfg.DialRTO, defaultConnectRTO)) * time.Millisecond
	p.connectRTO[ServerConn] = time.Duration(defaultInt(cfg.AcceptRTO, defaultConnectRTO)) * time.Millisecond
	p.connBuffer.r = uint32(defaultInt(cfg.ConnReadBuffer, defaultReadBuffer))
	p.connBuffer.w = uint32(defaultInt(cfg.ConnWriteBuffer, defaultWriteBuffer))
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

// net.Listener接口
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
	n, err := this.conn.WriteToUDP(data, conn.remoteInternetAddr)
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
	return this.ioBytes.w
}

// 连接指定的地址
func (this *RUDP) Dial(addr string, timeout time.Duration) (*Conn, error) {
	conn, err := this.newDialConn(addr)
	if err != nil {
		return nil, err
	}
	// 初始化dial消息
	msg := udpDataPool.Get().(*udpData)
	conn.writeMsgDial(msg)
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
			// 超时重发
			if now.Sub(conn.time) >= timeout {
				err = conn.netOpError("dial", opErr("timeout"))
				break Loop
			}
			this.WriteToConn(msg.buf[:msgDialLength], conn)
		case <-conn.connectSignal:
			// 建立连接结果通知
			switch conn.state {
			case connStateConnect:
				// 已经建立连接
				return conn, nil
			case connStateDial:
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
	conn.Close()
	// 移除dialing列表
	this.lock[ClientConn].Lock()
	delete(this.dialing, conn.cToken)
	this.lock[ClientConn].Unlock()
	//
	return nil, err
}

// 创建一个新的Conn变量
func (this *RUDP) newConn(cs, state uint8, rAddr *net.UDPAddr) *Conn {
	p := new(Conn)
	p.clientOrServer = cs
	p.state = state
	p.localListenAddr = this.conn.LocalAddr().(*net.UDPAddr)
	p.remoteInternetAddr = rAddr
	p.localMss = DetectMSS(rAddr)
	p.connectSignal = make(chan int, 1)
	p.closeSignal = make(chan struct{})
	p.time = time.Now()
	p.readQueue = readDataPool.Get().(*readData)
	p.writeQueue = writeDataPool.Get().(*writeData)
	p.readable = make(chan int, 1)
	p.writeable = make(chan int, 1)
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
	if uint32(len(this.dialing)) >= maxUint32 {
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
	if uint32(len(this.dialing)) >= maxUint32 {
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
				this.handleMsgData(ServerConn, msg)
			case msgDataS: // s->c，数据
				this.handleMsgData(ClientConn, msg)
			case msgAckC: // c->s，收到数据确认
				this.handleMsgAck(ServerConn, msg)
			case msgAckS: // s->c，收到数据确认
				this.handleMsgAck(ClientConn, msg)
			case msgCloseC: // c->s，关闭
				this.handleMsgClose(ServerConn, msg)
			case msgCloseS: // s->c，关闭
				this.handleMsgClose(ClientConn, msg)
			case msgPing: // c->s
				this.handleMsgPing(msg)
			case msgPong: // s->c
				this.handleMsgPong(msg)
			case msgInvalidC: // c<->s，无效的连接
				this.handleMsgInvalid(ServerConn, msg)
			case msgInvalidS: // c<->s，无效的连接
				this.handleMsgInvalid(ClientConn, msg)
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
	conn.writeMsgAccept(msg)
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
			// 超时重发
			if now.Sub(conn.time) >= timeout {
				return
			}
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
			conn.writeLock.Lock()
			// 最大发送个数（对方能接受的数据量，不能接受，则单发一个）
			n := maxInt(1, int(conn.remoteQueueRemains))
			// 没数据/超出最大发送个数
			p := conn.writeQueue.next
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
			conn.writeLock.Unlock()
			// 重置计时器
			timer.Reset(conn.rto)
		}
	}
}

// 关闭并释放Conn的资源
func (this *RUDP) closeConn(conn *Conn) {
	conn.Close()
	// connKey
	var key connKey
	key.Init(conn.remoteInternetAddr)
	key.cToken = conn.cToken
	key.sToken = conn.sToken
	// 移除connected列表
	this.lock[conn.clientOrServer].Lock()
	delete(this.connected[conn.clientOrServer], key)
	this.lock[conn.clientOrServer].Unlock()
}

// 获取已连接的Conn
func (this *RUDP) getConnectedConn(cs uint8, msg *udpData) (*Conn, bool) {
	var key connKey
	msg.InitConnKey(&key)
	this.lock[cs].RLock()
	conn, ok := this.connected[cs][key]
	this.lock[cs].RUnlock()
	return conn, ok
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
	cToken := binary.BigEndian.Uint32(msg.buf[msgDialToken:])
	// connKey
	var key connKey
	key.Init(msg.addr)
	key.cToken = cToken
	// 检查是否有accepting Conn
	this.lock[ServerConn].Lock()
	conn, ok := this.accepting[key]
	// 存在，这里不处理
	if ok {
		this.lock[ServerConn].Unlock()
		return
	}
	// 不存在，新的连接
	conn = this.newAcceptConn(cToken, msg.addr)
	// 创建失败
	if conn == nil {
		this.lock[ServerConn].Unlock()
		// 发送refuse消息
		msg.buf[msgType] = msgRefuse
		binary.BigEndian.PutUint32(msg.buf[msgRefuseToken:], cToken)
		this.WriteTo(msg.buf[:msgRefuseLength], msg.addr)
		return
	}
	// 添加到accepting列表
	key.sToken = conn.sToken
	this.accepting[key] = conn
	this.lock[ServerConn].Unlock()
	// 读取消息数据
	conn.readMsgDial(msg)
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
	// 检查是否有dialing Conn
	this.lock[ClientConn].RLock()
	conn, ok := this.dialing[cToken]
	this.lock[ClientConn].RUnlock()
	if !ok {
		// 发送msgInvalid，通知对方不要在发送msgAccept了
		this.writeMsgInvalid(msg, ClientConn)
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateDial:
		// 保存sToken
		conn.sToken = sToken
		// 修改状态
		conn.state = connStateConnect
		// 计算rto
		conn.calcRTO(time.Now().Sub(conn.time))
		conn.lock.Unlock()
		// connKey
		var key connKey
		key.Init(conn.remoteInternetAddr)
		key.cToken = cToken
		key.sToken = sToken
		// 添加到connected列表
		this.lock[ClientConn].Lock()
		this.connected[ClientConn][key] = conn
		this.lock[ClientConn].Unlock()
		// 读取消息数据
		conn.readMsgDial(msg)
		// 通知连接
		conn.connectSignal <- 1
		// 启动客户端Conn写协程
		go this.connWriteRoutine(conn)
	case connStateConnect:
		// 已经是连接状态，说明是重复的消息
		conn.lock.Unlock()
	default:
		// 其他状态不处理
		conn.lock.Unlock()
		// 发送msgInvalid，通知对方不要在发送msgAccept了
		this.writeMsgInvalid(msg, ClientConn)
		return
	}
	// 发送msgConnect
	conn.writeMsgConnect(msg)
	this.WriteToConn(msg.buf[:msgConnectLength], conn)
}

// 处理msgRefuse
func (this *RUDP) handleMsgRefuse(msg *udpData) {
	// 检查消息大小和版本
	if msg.len != msgRefuseLength || msg.buf[msgRefuseVersion] != msgVersion {
		return
	}
	// 客户端token
	cToken := binary.BigEndian.Uint32(msg.buf[msgRefuseToken:])
	// 检查是否有dialing Conn
	this.lock[ClientConn].RLock()
	conn, ok := this.dialing[cToken]
	this.lock[ClientConn].RUnlock()
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
		conn.connectSignal <- 1
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
	if this.accepted == nil {
		return
	}
	// 检查消息大小
	if msg.len != msgConnectLength {
		return
	}
	// connKey
	var key connKey
	msg.InitConnKey(&key)
	// 检查是否有accepting Conn
	this.lock[ServerConn].RLock()
	conn, ok := this.accepting[key]
	this.lock[ServerConn].RUnlock()
	// 不存在不处理
	if !ok {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connStateAccept:
		// 修改状态
		conn.state = connStateConnect
		// 计算rto
		conn.calcRTO(time.Now().Sub(conn.time))
		conn.lock.Unlock()
		// 添加到connected列表
		this.lock[ServerConn].Lock()
		this.connected[ServerConn][key] = conn
		this.lock[ServerConn].Unlock()
		// 通知
		conn.connectSignal <- 1
	default:
		// 其他不处理
		// connStateConnect，重复消息
		// connStateDial，服务端不可能
		// connStateClose，关闭了
		conn.lock.Unlock()
	}
}

// 处理msgPing
func (this *RUDP) handleMsgPing(msg *udpData) {
	if msg.len != msgPingLength {
		return
	}
	conn, ok := this.getConnectedConn(ServerConn, msg)
	if !ok {
		this.writeMsgInvalid(msg, msgInvalidC)
		return
	}
	conn.writeMsgPong(msg, binary.BigEndian.Uint32(msg.buf[msgPingId:]))
	this.WriteToConn(msg.buf[:msgPongLength], conn)
}

// 处理msgPong
func (this *RUDP) handleMsgPong(msg *udpData) {
	conn, ok := this.getConnectedConn(ClientConn, msg)
	if ok {
		conn.readMsgPong(msg)
	}
}

// 处理msgData
func (this *RUDP) handleMsgData(cs uint8, msg *udpData) {
	if msg.len <= msgDataPayload {
		return
	}
	conn, ok := this.getConnectedConn(cs, msg)
	if !ok {
		this.writeMsgInvalid(msg, cs)
		return
	}
	conn.readMsgData(msg)
	this.WriteToConn(msg.buf[:msgAckLength], conn)
}

// 处理msgAck
func (this *RUDP) handleMsgAck(cs uint8, msg *udpData) {
	if msg.len != msgAckLength {
		return
	}
	conn, ok := this.getConnectedConn(cs, msg)
	if ok {
		conn.readMsgAck(msg)
	}
}

// 处理msgClose
func (this *RUDP) handleMsgClose(cs uint8, msg *udpData) {
	if msg.len != msgCloseLength {
		return
	}
	conn, ok := this.getConnectedConn(cs, msg)
	if ok {
		this.closeConn(conn)
	}
	this.writeMsgInvalid(msg, cs)
}

// 处理msgInvalid
func (this *RUDP) handleMsgInvalid(cs uint8, msg *udpData) {
	if msg.len != msgInvalidLength {
		return
	}
	conn, ok := this.getConnectedConn(cs, msg)
	if ok {
		this.closeConn(conn)
	}
}

// 发送msgInvalidC/msgInvalidS
func (this *RUDP) writeMsgInvalid(msg *udpData, cs uint8) {
	msg.buf[msgType] = msgInvalid[cs]
	this.WriteTo(msg.buf[:msgInvalidLength], msg.addr)
}
