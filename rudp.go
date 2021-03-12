package rudp

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"
)

var (
	readQueueCap   = uint16(0xffff / 2)     // 默认的读缓存队列最大长度
	writeQueueCap  = uint16(0xffff / 2)     // 默认的写缓冲队列最大长度
	handleQueueLen = uint16(1024)           // 默认的conn的处理队列长度
	connectRTO     = 100 * time.Millisecond // connect segment超时重传，毫秒
)

// 设置默认的conn的读缓存队列最大长度
func SetReadQueueMaxLength(n uint16) {
	readQueueCap = n
}

// 设置默认的conn的写缓冲队列最大长度
func SetWriteQueueMaxLength(n uint16) {
	writeQueueCap = n
}

// 设置默认的conn的segment处理队列长度
func SetHandleQueueLength(n uint16) {
	handleQueueLen = n
}

// 使用配置值创建一个新的RUDP，server能力需要配置Config.AcceptQueue
func Listen(address string) (*RUDP, error) {
	// 解析地址
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	// 绑定地址
	conn, err := net.ListenUDP(addr.Network(), addr)
	if err != nil {
		return nil, err
	}
	// 初始化成员变量
	r := new(RUDP)
	r.conn = conn
	r.quitSignal = make(chan struct{})
	r.connectRTO = connectRTO
	r.connHandleQueue = handleQueueLen
	r.connReadQueueCap = readQueueCap
	r.connWriteQueueCap = writeQueueCap
	// 使用的是server lock
	r.acceptCond.L = &r.lock[1]
	for i := 0; i < len(r.connection); i++ {
		r.connection[i] = make(map[connKey]*Conn)
	}
	// 读取udp数据并处理
	for i := 0; i < runtime.NumCPU(); i++ {
		r.waitGroup.Add(1)
		go r.handleUDPDataRoutine()
	}
	return r, nil
}

// RUDP是一个同时具备C/S能力的引擎
type RUDP struct {
	conn              *net.UDPConn         // 底层socket
	waitGroup         sync.WaitGroup       // 等待所有协程退出
	quitSignal        chan struct{}        // 通知所有协程退出的信号
	closed            bool                 // 是否调用了Close()
	ioBytes           [2]uint64            // io总字节，0:read，1:write
	lock              [2]sync.RWMutex      // 相关数据的锁，0:client，1:server
	connection        [2]map[connKey]*Conn // 所有的Conn，0:client，1:server
	acceptConn        list.List            // 已经建立连接的Conn，等待Accept()调用
	acceptCond        sync.Cond            // 等待client连接的信号
	fec               byte                 // 新连接纠错
	crypto            byte                 // 新的连接加密的算法
	connectRTO        time.Duration        // 建立连接，connect segment的超时重发
	connHandleQueue   uint16               // 新连接的segment处理队列大小
	connReadQueueCap  uint16               // 新连接的segment处理队列大小
	connWriteQueueCap uint16               // 新连接的segment处理队列大小
}

// 设置新的连接是否启用纠错
func (r *RUDP) EnableFEC(_type byte) {
	r.fec = _type
}

// 设置新的连接是否启用加密
func (r *RUDP) EnableCrypto(_type byte) {
	r.crypto = _type
}

// 建立连接，发送connect segment的超时重传，最小1ms
func (r *RUDP) SetconnectRTO(timeout time.Duration) {
	if timeout < time.Millisecond {
		r.connectRTO = time.Millisecond
	} else {
		r.connectRTO = timeout
	}
}

// 新的连接的读队列最大长度
func (r *RUDP) SetConnReadQueueCap(n uint16) {
	r.connReadQueueCap = n
}

// 新的连接的写队列最大长度
func (r *RUDP) SetConnWriteQueueCap(n uint16) {
	r.connWriteQueueCap = n
}

// net.Listener接口
func (r *RUDP) Addr() net.Addr {
	return r.conn.LocalAddr()
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
	close(r.quitSignal)
	r.acceptCond.Broadcast()
	// 关闭所有的Conn
	for i := 0; i < len(r.connection); i++ {
		for _, v := range r.connection[i] {
			v.Close()
		}
	}
	// 等待所有协程退出
	r.waitGroup.Wait()
	return nil
}

// net.Listener接口
func (r *RUDP) Accept() (net.Conn, error) {
	r.acceptCond.L.Lock()
	for !r.closed {
		if r.acceptConn.Len() < 1 {
			r.acceptCond.Wait()
			continue
		}
		v := r.acceptConn.Remove(r.acceptConn.Front())
		conn := v.(*Conn)
		conn.state = connectState
		r.acceptCond.L.Unlock()
		return v.(*Conn), nil
	}
	r.acceptCond.L.Unlock()
	return nil, r.netOpError("accept", rudpClosedError)
}

// 连接指定的地址，对于一个地址，可以创建2^32个连接
func (r *RUDP) Dial(address string, timeout time.Duration) (*Conn, error) {
	// 解析地址
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	// 新的client Conn
	conn, err := r.newDialConn(addr)
	if err != nil {
		return nil, err
	}
	// timeout timer
	timer := time.NewTimer(r.connectRTO)
	defer timer.Stop()
	// 初始化dial handshake segment
	var buff [maxMSS]byte
	r.encHandshakeSegment(conn, buff[:], dialSegment, timeout)
	for err == nil {
		select {
		case <-time.After(timeout): // 连接超时
			err = new(timeoutError)
		case <-timer.C: // 超时重传
			r.WriteToConn(buff[:handshakeSegmentLength], conn)
			timer.Reset(r.connectRTO)
		case <-conn.connectSignal: // 连接信号
			switch conn.state {
			case connectState: // 已连接，返回
				r.waitGroup.Add(2)
				go r.clientConnRoutine(conn)
				go r.connHandleDataRoutine(conn)
				return conn, nil
			case dialState: // 拒绝连接，返回
				err = errors.New("connect reject")
			default:
				err = connClosedError
			}
		case <-conn.quitSignal: // Conn关闭信号
			err = connClosedError
		case <-r.quitSignal: // RUDP关闭信号
			err = rudpClosedError
		}
	}
	r.freeConn(conn)
	return nil, conn.netOpError("dial", err)
}

func (r *RUDP) encHandshakeSegment(c *Conn, b []byte, segType byte, timeout time.Duration) {
	b[segmentType] = segType
	b[handshakeSegmentVersion] = protolVersion
	binary.BigEndian.PutUint32(b[handshakeSegmentToken:], c.writeQueue.token)
	binary.BigEndian.PutUint64(b[handshakeSegmentTimestamp:], c.timestamp)
	copy(b[handshakeSegmentLocalIP:], c.listenAddr[0].IP.To16())
	binary.BigEndian.PutUint16(b[handshakeSegmentLocalPort:], uint16(c.listenAddr[0].Port))
	copy(b[handshakeSegmentRemoteIP:], c.internetAddr[1].IP.To16())
	binary.BigEndian.PutUint16(b[handshakeSegmentRemotePort:], uint16(c.internetAddr[1].Port))
	binary.BigEndian.PutUint64(b[handshakeSegmentTimestamp:], uint64(timeout))
	b[handshakeSegmentFEC] = r.fec
	b[handshakeSegmentCrypto] = r.crypto
	c.diffieHellman.ExchangeKey(b[handshakeSegmentExchangeKey:handshakeSegmentLength])
}

// 使用net.UDPConn向指定地址发送指定数据
func (r *RUDP) WriteTo(data []byte, addr *net.UDPAddr) (int, error) {
	n, err := r.conn.WriteToUDP(data, addr)
	if err == nil {
		r.ioBytes[1] += uint64(n)
	}
	return n, err
}

// 使用net.UDPConn向指定地址发送指定数据
func (r *RUDP) WriteToConn(data []byte, conn *Conn) (int, error) {
	n, err := r.conn.WriteToUDP(data, conn.internetAddr[1])
	if err == nil {
		conn.ioBytes[1] += uint64(n)
		r.ioBytes[1] += uint64(n)
	}
	return n, err
}

// 返回net.UDPConn的本地地址
func (r *RUDP) LocalAddr() net.Addr {
	return r.conn.LocalAddr()
}

// 读取的总字节
func (r *RUDP) ReadBytes() uint64 {
	return r.ioBytes[0]
}

// 发送的总字节
func (r *RUDP) WriteBytes() uint64 {
	return r.ioBytes[1]
}

// 返回*net.OpError
func (r *RUDP) netOpError(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "udp",
		Source: nil,
		Addr:   r.conn.LocalAddr(),
		Err:    err,
	}
}

func (r *RUDP) handleUDPDataRoutine() {
	r.waitGroup.Add(1)
	defer r.waitGroup.Done()
	var err error
	key := new(connKey)
	// seg := new(segment)
	for !r.closed {
		seg := segmentPool.Get().(*segment)
		// 读取数据并处理
		seg.n, seg.a, err = r.conn.ReadFromUDP(seg.b[:])
		if err != nil {
			// todo: 日志
			continue
		}
		r.ioBytes[0] += uint64(seg.n)
		key.Init(seg.a)
		// 处理
		switch seg.b[0] {
		case dialSegment:
			r.handleDialSegment(seg, key)
			segmentPool.Put(seg)
		case acceptSegment:
			r.handleAcceptSegment(seg, key)
			segmentPool.Put(seg)
		case handshakeSuccessSegment:
			r.handleHandshakeSuccessSegment(seg, key)
			segmentPool.Put(seg)
		case pingSegment:
			r.handlePingSegment(seg, key)
			segmentPool.Put(seg)
		case pongSegment:
			r.handlePongSegment(seg, key)
			segmentPool.Put(seg)
		case invalidSegment:
			r.handleInvalidSegment(seg, key, 1)
			segmentPool.Put(seg)
		case serverInvalidSegment:
			r.handleInvalidSegment(seg, key, 0)
			segmentPool.Put(seg)
		case clientDataSegment, clientAckSegment, clientDiscardSegment, clientDiscardAckCmdSegment:
			r.handleDataSegment(seg, key, 1)
		case serverDataSegment, serverAckSegment, serverDiscardSegment, serverDiscardAckCmdSegment:
			r.handleDataSegment(seg, key, 0)
		default:
			segmentPool.Put(seg)
		}
	}
}

func (r *RUDP) handleDialSegment(seg *segment, key *connKey) {
	// 检查segment
	if seg.b[handshakeSegmentVersion] != protolVersion ||
		seg.n != handshakeSegmentLength {
		return
	}
	key.token = binary.BigEndian.Uint32(seg.b[handshakeSegmentToken:])
	timestamp := binary.BigEndian.Uint64(seg.b[handshakeSegmentTimestamp:])
	// 获取Conn
	r.lock[1].Lock()
	conn, ok := r.connection[1][*key]
	// 没有找到相应的Conn，添加
	if !ok || timestamp > conn.timestamp {
		conn = new(Conn)
		conn.timestamp = timestamp
		r.connection[1][*key] = conn
		r.lock[1].Unlock()
		r.waitGroup.Add(1)
		go r.serverConnRoutine(conn, seg.a, key.token, time.Duration(binary.BigEndian.Uint64(seg.b[handshakeSegmentTimeout:])), timestamp)
		return
	}
	// 有相应的Conn，但是是一个新的dial（timestamp比原来的大）
	if timestamp > conn.timestamp {
		newConn := new(Conn)
		newConn.timestamp = timestamp
		r.connection[1][*key] = newConn
		r.lock[1].Unlock()
		r.waitGroup.Add(1)
		go r.serverConnRoutine(newConn, seg.a, key.token, time.Duration(binary.BigEndian.Uint64(seg.b[handshakeSegmentTimeout:])), timestamp)
		// 关闭原来的
		r.freeConn(conn)
	} else {
		r.lock[1].Unlock()
	}
}

func (r *RUDP) handleAcceptSegment(seg *segment, key *connKey) {
	// 检查数据
	if seg.b[handshakeSegmentVersion] != protolVersion ||
		seg.n != handshakeSegmentLength {
		return
	}
	// 获取Conn
	key.token = binary.BigEndian.Uint32(seg.b[handshakeSegmentToken:])
	r.lock[0].RLock()
	conn, ok := r.connection[0][*key]
	r.lock[0].RUnlock()
	if !ok {
		// 不存在响应invalidSegment
		r.writeInvalidSegment(seg, invalidSegment, key.token)
		return
	}
	// 处理
	conn.lock.Lock()
	switch conn.state {
	case dialState:
		// 修改状态
		conn.state = connectState
		conn.lock.Unlock()
		// 发送信号
		close(conn.connectSignal)
		// 响应handshakeSuccessSegment
		r.writeHandshakeSuccessSegment(seg, key.token)
	case connectState:
		conn.lock.Unlock()
		// 已经连接，响应handshakeSuccessSegment
		r.writeHandshakeSuccessSegment(seg, key.token)
	default:
		conn.lock.Unlock()
		// 状态不对，响应invalidSegment
		r.writeInvalidSegment(seg, invalidSegment, key.token)
	}
}

func (r *RUDP) handleHandshakeSuccessSegment(seg *segment, key *connKey) {
	// 检查数据
	if seg.n != handshakeSuccessSegmentLength {
		return
	}
	// 获取Conn
	key.token = binary.BigEndian.Uint32(seg.b[handshakeSuccessSegmentToken:])
	r.lock[1].RLock()
	conn, ok := r.connection[1][*key]
	r.lock[1].RUnlock()
	if !ok {
		// 不存在响应invalidSegment
		r.writeInvalidSegment(seg, invalidSegment, key.token)
		return
	}
	// 处理
	conn.lock.Lock()
	switch conn.state {
	case dialState:
		// 修改状态
		conn.state = connectState
		conn.lock.Unlock()
		// 发送连接信号
		close(conn.connectSignal)
	default:
		conn.lock.Unlock()
		// 状态不对，响应invalidSegment
		r.writeInvalidSegment(seg, invalidSegment, key.token)
	}
}

func (r *RUDP) handleDataSegment(seg *segment, key *connKey, from byte) {
	// 获取Conn
	key.token = binary.BigEndian.Uint32(seg.b[dataSegmentToken:])
	r.lock[from].RLock()
	conn, ok := r.connection[from][*key]
	r.lock[from].RUnlock()
	if !ok {
		r.writeInvalidSegment(seg, (from<<7)|invalidSegment, key.token)
		return
	}
	select {
	case conn.handleQueue <- seg:
	default:
	}
}

func (r *RUDP) handleInvalidSegment(seg *segment, key *connKey, from byte) {
	// 检查数据
	if seg.n != invalidSegmentLength {
		return
	}
	key.token = binary.BigEndian.Uint32(seg.b[invalidSegmentToken:])
	r.lock[from].RLock()
	conn, ok := r.connection[from][*key]
	r.lock[from].RUnlock()
	if ok {
		conn.Close()
	}
}

func (r *RUDP) handlePingSegment(seg *segment, key *connKey) {

}

func (r *RUDP) handlePongSegment(seg *segment, key *connKey) {

}

func (r *RUDP) writeHandshakeSuccessSegment(seg *segment, token uint32) {
	seg.b[0] = handshakeSuccessSegment
	binary.BigEndian.PutUint32(seg.b[handshakeSuccessSegmentToken:], token)
	r.WriteTo(seg.b[:handshakeSuccessSegmentLength], seg.a)
}

func (r *RUDP) writeInvalidSegment(seg *segment, segType byte, token uint32) {
	seg.b[0] = segType
	binary.BigEndian.PutUint32(seg.b[invalidSegmentToken:], token)
	r.WriteTo(seg.b[:invalidSegmentLength], seg.a)
}

func (r *RUDP) newDialConn(addr *net.UDPAddr) (*Conn, error) {
	// 探测mss
	mss, err := detectMSS(addr)
	if err != nil {
		return nil, err
	}
	// 注册到connection表
	var key connKey
	key.Init(addr)
	key.token = mathRand.Uint32()
	// 初始token
	token := key.token
	var conn *Conn
	var ok bool
	// 检查没有使用的token
	r.lock[0].Lock()
	for {
		// 已关闭
		if r.closed {
			return nil, r.netOpError("dial", rudpClosedError)
		}
		_, ok = r.connection[0][key]
		if !ok {
			conn = new(Conn)
			r.connection[0][key] = conn
			break
		}
		key.token++
		if key.token == token {
			return nil, r.netOpError("dial", fmt.Errorf("too many connections to %s", addr.String()))
		}
	}
	r.lock[0].Unlock()
	// 初始化Conn
	r.initConn(conn, addr, 0, dialState, key.token, uint64(time.Now().Unix()), mss)
	return conn, nil
}

func (r *RUDP) initConn(conn *Conn, addr *net.UDPAddr, from, state byte, token uint32, timestamp uint64, mss uint16) {
	conn.rudp = r
	conn.state = state
	conn.quitSignal = make(chan struct{})
	conn.connectSignal = make(chan struct{})
	conn.timestamp = timestamp
	conn.handleQueue = make(chan *segment, r.connHandleQueue)
	// 交换密钥算法
	conn.diffieHellman = newDiffieHellman()
	// 地址
	conn.listenAddr[0] = r.conn.LocalAddr().(*net.UDPAddr)
	conn.listenAddr[1] = new(net.UDPAddr)
	conn.internetAddr[0] = new(net.UDPAddr)
	conn.internetAddr[1] = addr
	// 接收队列
	conn.readQueue.enable = make(chan byte, 1)
	conn.readQueue.cap = r.connReadQueueCap
	// 发送队列
	conn.writeQueue.from = from
	conn.writeQueue.token = token
	conn.writeQueue.enable = make(chan byte, 1)
	conn.writeQueue.cap = r.connWriteQueueCap
	conn.writeQueue.rto.min = minRTO
	conn.writeQueue.rto.max = maxRTO
	conn.writeQueue.mss = mss
}

func (r *RUDP) freeConn(conn *Conn) {
	r.waitGroup.Add(1)
	go func() {
		defer r.waitGroup.Done()
		conn.lock.Lock()
		if conn.state == closedState {
			conn.lock.Unlock()
			return
		}
		conn.state = closedState
		conn.lock.Unlock()
		// 移除
		var key connKey
		key.Init(conn.internetAddr[1])
		key.token = conn.writeQueue.token
		from := conn.writeQueue.from >> 7
		r.lock[from].Lock()
		delete(r.connection[from], key)
		r.lock[from].Unlock()
		// 释放资源
		close(conn.quitSignal)
		close(conn.readQueue.enable)
		close(conn.writeQueue.enable)
		conn.readQueue.Release()
		conn.writeQueue.Release()
		for seg := range conn.handleQueue {
			segmentPool.Put(seg)
		}
	}()
}

func (r *RUDP) serverConnRoutine(conn *Conn, addr *net.UDPAddr, token uint32, timeout time.Duration, timestamp uint64) {
	defer func() {
		r.freeConn(conn)
		r.waitGroup.Done()
	}()
	// 探测mss
	mss, err := detectMSS(addr)
	if err != nil {
		// todo log
		return
	}
	// 初始化Conn
	r.initConn(conn, addr, 1, dialState, token, timestamp, mss)
	// 计时器
	timer := time.NewTimer(r.connectRTO)
	defer timer.Stop()
	// 先发送accept
	var buff [maxMSS]byte
	r.encHandshakeSegment(conn, buff[:], acceptSegment, timeout)
Loop:
	for {
		select {
		case <-time.After(timeout): // 连接超时
			return
		case <-timer.C: // 超时重传
			r.WriteToConn(buff[:handshakeSegmentLength], conn)
			timer.Reset(r.connectRTO)
		case <-r.quitSignal: // RUDP关闭信号
			return
		case <-conn.quitSignal: // Conn关闭信号
			return
		case <-conn.connectSignal: // 建立连接信号
			switch conn.state {
			case connectState: // 已连接，返回
				break Loop
			}
			return
		}
	}
	// 加入acceptConn
	r.acceptCond.L.Lock()
	r.acceptConn.PushBack(conn)
	r.acceptCond.L.Unlock()
	r.acceptCond.Broadcast()
	// 重置计时器
	timer.Reset(conn.writeQueue.rto.rto)
	for {
		select {
		case <-r.quitSignal: // RUDP关闭信号
			return
		case <-conn.quitSignal: // Conn关闭信号
			return
		case now := <-timer.C: // 超时重传检查
			conn.retransmission(&now)
			timer.Reset(conn.writeQueue.rto.rto)
		}
	}
}

func (r *RUDP) clientConnRoutine(conn *Conn) {
	timer := time.NewTimer(conn.writeQueue.rto.rto)
	defer func() {
		timer.Stop()
		r.freeConn(conn)
		r.waitGroup.Done()
	}()
	for {
		select {
		case <-r.quitSignal: // RUDP关闭信号
			return
		case <-conn.quitSignal: // Conn关闭信号
			return
		case now := <-timer.C: // 超时重传检查
			conn.retransmission(&now)
			timer.Reset(conn.writeQueue.rto.rto)
		}
	}
}

func (r *RUDP) connHandleDataRoutine(conn *Conn) {
	defer r.waitGroup.Done()
	for {
		seg, ok := <-conn.handleQueue
		if !ok {
			return
		}
		switch seg.b[0] {
		case clientDataSegment, serverDataSegment:
			conn.readQueue.Lock()
			conn.readQueue.AddData(binary.BigEndian.Uint16(seg.b[dataSegmentSN:]), seg.b[dataSegmentPayload:seg.n])
			conn.readQueue.Unlock()
		case clientAckSegment, serverAckSegment:
			if seg.n == ackSegmentLength {
				conn.writeQueue.Lock()

				conn.writeQueue.Unlock()
			}
		case clientDiscardSegment, serverDiscardSegment:
			if seg.n == discardSegmentLength {
				conn.readQueue.Lock()
				conn.readQueue.Discard(binary.BigEndian.Uint16(seg.b[discardSegmentSN:]),
					binary.BigEndian.Uint16(seg.b[discardSegmentBegin:]), binary.BigEndian.Uint16(seg.b[discardSegmentEnd:]))
				conn.readQueue.Unlock()
			}
		case clientDiscardAckCmdSegment, serverDiscardAckCmdSegment:
			if seg.n == discardAckSegmentLength {
				conn.writeQueue.Lock()

				conn.writeQueue.Unlock()
			}
		}
		segmentPool.Put(seg)
	}
}
