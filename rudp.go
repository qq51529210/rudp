package rudp

import (
	"encoding/binary"
	"math"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"time"
)

var (
	mathRand    = rand.New(rand.NewSource(time.Now().Unix())) // 随机数
	readBuffer  = 1024 * 400                                  // 默认的读缓冲区，400k
	writeBuffer = 1024 * 400                                  // 默认的写缓冲区，400k
	minRTO      = 10                                          // 最小超时重传，毫秒
	maxRTO      = 100                                         // 最大超时重传，毫秒
	dialRTO     = 100 * time.Millisecond                      // dialc超时重传
	DetectMSS   = func(*net.UDPAddr) uint16 { return maxMSS } // 返回mss和rtt
)

// 如果n1<n2，返回n2
func maxInt(n1, n2 int) int {
	if n1 > n2 {
		return n1
	}
	return n2
}

// 如果n1<n2，返回n1
func minInt(n1, n2 int) int {
	if n1 > n2 {
		return n2
	}
	return n1
}

type opErr string

func (e opErr) Error() string   { return string(e) }
func (e opErr) Timeout() bool   { return string(e) == "timeout" }
func (e opErr) Temporary() bool { return false }

type closeErr string

func (e closeErr) Error() string   { return string(e) + " has been closed" }
func (e closeErr) Timeout() bool   { return false }
func (e closeErr) Temporary() bool { return false }

// 连接池的键，因为客户端可能在nat后面，所以加上token
type connectKey struct {
	ip1   uint64 // IPV6地址字符数组前64位
	ip2   uint64 // IPV6地址字符数组后64位
	port  uint16 // 端口
	token uint32 // token
}

func (this *connectKey) Init(addr *net.UDPAddr) {
	if len(addr.IP) == net.IPv4len {
		this.ip1 = 0
		this.ip2 = uint64(0xff)<<40 | uint64(0xff)<<32 |
			uint64(addr.IP[0])<<24 |
			uint64(addr.IP[1])<<16 |
			uint64(addr.IP[2])<<8 |
			uint64(addr.IP[3])
	} else {
		this.ip1 = binary.BigEndian.Uint64(addr.IP[0:])
		this.ip2 = binary.BigEndian.Uint64(addr.IP[8:])
	}
	this.port = uint16(addr.Port)
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
	r.quit = make(chan struct{})
	r.acceptCond.L = &r.lock[1]
	r.readBuffer = readBuffer
	r.writeBuffer = writeBuffer
	for i := 0; i < 2; i++ {
		r.token[i] = mathRand.Uint32()
		r.connection[i] = make(map[connectKey]*Conn)
		r.connected[i] = make(map[uint32]*Conn)
	}
	for i := 0; i < runtime.NumCPU(); i++ {
		go r.handleUDPDataRoutine()
	}
	return r, nil
}

// RUDP是一个同时具备C/S能力的引擎
type RUDP struct {
	conn         *net.UDPConn            // 底层socket
	wait         sync.WaitGroup          // 等待所有协程退出
	quit         chan struct{}           // 通知所有协程退出的信号
	acceptCond   sync.Cond               // 等待client连接的信号
	closed       bool                    // 是否调用了Close()
	readBytes    uint64                  // udp读的总字节
	writeBytes   uint64                  // udp写的总字节
	lock         [2]sync.RWMutex         // 相关数据的锁，0:client，1:server
	token        [2]uint32               // 建立连接起始随机数，递增，0:client，1:server
	connection   [2]map[connectKey]*Conn // 所有Conn，包括正在建立和已经建立的，0:client，1:server
	accepting    map[uint32]*Conn        // 已经建立连接的Conn，等待Accept()调用
	connected    [2]map[uint32]*Conn     // 已经建立连接的Conn，0:client，1:server
	readBuffer   int                     // 新的连接的读队列大小
	writeBuffer  int                     // 新的连接的写队列大小
	enableEFC    bool                    // 新的连接是否启用纠错
	enableCrypto bool                    // 新的连接是否启用加密
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
	close(r.quit)
	r.acceptCond.Broadcast()
	// 关闭所有的Conn
	for i := 0; i < 2; i++ {
		r.lock[i].Lock()
		for _, v := range r.connection[i] {
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
	r.acceptCond.L.Lock()
	for !r.closed {
		if len(r.connection) < 1 {
			r.acceptCond.Wait()
			continue
		}
		for _, v := range r.connection[1] {
			r.acceptCond.L.Unlock()
			return v, nil
		}
	}
	r.acceptCond.L.Unlock()
	return nil, r.netOpError("accept", closeErr("rudp"))
}

// 连接指定的地址
func (r *RUDP) Dial(address string, timeout time.Duration) (*Conn, error) {
	// client Conn
	conn, err := r.newDialConn(address)
	if err != nil {
		return nil, err
	}
	// connect segment
	var buff [maxMSS]byte
	conn.encConnectSegment(buff[:], cmdSegmentTypeDial)
	// timeout timer
	timer := time.NewTimer(dialRTO)
	defer timer.Stop()
	dialTime := time.Now()
	for {
		select {
		case now := <-timer.C: // 超时重发
			conn.rtt = now.Sub(dialTime)
			if conn.rtt >= timeout {
				r.freeConn(conn)
				return nil, conn.netOpError("dial", opErr("timeout"))
			}
			// 检查Conn.state
			switch conn.state {
			case connectedState:
				// 已连接，返回
				conn.rttVar = conn.rtt
				return conn, nil
			case connectingState:
				// 未连接，继续发送
				r.WriteToConn(buff[:connectSegmentLength], conn)
				timer.Reset(dialRTO)
			default:
				// 关闭连接，返回
				r.freeConn(conn)
				return nil, conn.netOpError("dial", opErr("connect refuse"))
			}
		case <-r.quit: // RUDP被关闭
			err = conn.netOpError("dial", closeErr("rudp"))
		}
	}
}

// 使用net.UDPConn向指定地址发送指定数据
func (r *RUDP) WriteTo(data []byte, addr *net.UDPAddr) (int, error) {
	n, err := r.conn.WriteToUDP(data, addr)
	if err == nil {
		r.writeBytes += uint64(n)
	}
	return n, err
}

// 使用net.UDPConn向指定地址发送指定数据
func (r *RUDP) WriteToConn(data []byte, conn *Conn) (int, error) {
	n, err := r.conn.WriteToUDP(data, conn.remoteInternetAddr)
	if err == nil {
		conn.writeBytes += uint64(n)
		r.writeBytes += uint64(n)
	}
	return n, err
}

// 返回net.UDPConn的本地地址
func (r *RUDP) LocalAddr() net.Addr {
	return r.conn.LocalAddr()
}

// 读取的总字节
func (r *RUDP) ReadBytes() uint64 {
	return r.readBytes
}

// 发送的总字节
func (r *RUDP) WriteBytes() uint64 {
	return r.writeBytes
}

// 设置新的Conn的读缓存
func (r *RUDP) SetConnReadBuffer(size int) {
	r.readBuffer = maxInt(size, readBuffer)
}

// 设置新的Conn的写缓存
func (r *RUDP) SetConnWriteBuffer(size int) {
	r.writeBuffer = maxInt(size, writeBuffer)
}

// 设置新的连接是否启用纠错
func (r *RUDP) EnableEFC(enable bool) {
	r.enableEFC = enable
}

// 设置新的连接是否启用加密
func (r *RUDP) EnableCrypto(enable bool) {
	r.enableCrypto = enable
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

// 读udp数据的协程
func (r *RUDP) handleUDPDataRoutine() {
	r.wait.Add(1)
	defer r.wait.Done()
	var err error
	var seg segment
	for !r.closed {
		// 读取数据并处理
		seg.n, seg.a, err = r.conn.ReadFromUDP(seg.b[:])
		if err != nil {
			// todo: 日志
			continue
		}
		r.readBytes += uint64(seg.n)
		// 处理
		switch seg.b[0] & 0b01100000 {
		case cmdSegment:
			switch seg.b[cmdSegmentType] {
			case cmdSegmentTypeDial:
				r.handleDialSegment(&seg)
			case cmdSegmentTypeAccept:
				r.handleAcceptSegment(&seg)
			case cmdSegmentTypeReject:
				r.handleRejectSegment(&seg)
			case cmdSegmentTypeClose:
				r.handleCloseSegment(&seg)
			case cmdSegmentTypeInvalid:
				r.handleInvalidSegment(&seg)
			}
		case dataSegment:
			r.handleDataSegment(&seg)
		case ackSegment:
			r.handleAckSegment(&seg)
		}
	}
}

// server处理
func (r *RUDP) handleDialSegment(seg *segment) {
	// 检查数据
	if seg.b[0]>>7 != 0 || seg.b[connectSegmentVersion] != protolVersion || seg.n != connectSegmentLength {
		return
	}
	// client token
	cToken := binary.BigEndian.Uint32(seg.b[connectSegmentClientToken:])
	conn, ok := r.newAcceptConn(cToken, seg.a)
	if conn == nil {
		// 发送reject
		seg.b[segmentHeader] = cmdSegment
		seg.b[cmdSegmentType] = cmdSegmentTypeReject
		seg.b[rejectSegmentVersion] = protolVersion
		binary.BigEndian.PutUint32(seg.b[rejectSegmentClientToken:], cToken)
		r.WriteTo(seg.b[:rejectSegmentLength], seg.a)
		return
	}
	if !ok {
		// 读取字段
		conn.decConnectSegment(seg.b[:], readBuffer)
	}
	// 发送connecting
	conn.encConnectSegment(seg.b[:], cmdSegmentTypeAccept)
	r.WriteTo(seg.b[:connectSegmentLength], seg.a)
}

// client处理
func (r *RUDP) handleAcceptSegment(seg *segment) {
	// 检查数据
	if seg.b[0]>>7 != 1 || seg.b[connectSegmentVersion] != protolVersion || seg.n != connectSegmentLength {
		return
	}
	// connection Conn
	var key connectKey
	key.Init(seg.a)
	key.token = binary.BigEndian.Uint32(seg.b[connectSegmentClientToken:])
	r.lock[0].RLock()
	conn, ok := r.connection[0][key]
	r.lock[0].RUnlock()
	if !ok {
		return
	}
	// 检查状态
	if conn.state == connectingState {
		conn.sToken = binary.BigEndian.Uint32(seg.b[connectSegmentServerToken:])
	}
	conn.lock.Lock()
	switch conn.state {
	case connectingState:
		// 保存sToken
		conn.sToken = binary.BigEndian.Uint32(seg.b[connectSegmentServerToken:])
		// 添加到connected
		key.token = conn.sToken
		r.lock[0].Lock()
		r.connected[0][conn.sToken] = conn
		r.lock[0].Unlock()
		// 读取字段
		conn.decConnectSegment(seg.b[:], r.readBuffer)
		// 修改状态
		conn.state = connectedState
		// 启动客户端Conn写协程
		go r.connRoutine(conn)
	}
	conn.lock.Unlock()
}

// client处理
func (r *RUDP) handleRejectSegment(seg *segment) {
	// 检查数据
	if seg.b[0]>>7 != 1 || seg.b[connectSegmentVersion] != protolVersion || seg.n != rejectSegmentLength {
		return
	}
	// connection Conn
	var key connectKey
	key.Init(seg.a)
	key.token = binary.BigEndian.Uint32(seg.b[rejectSegmentClientToken:])
	r.lock[0].RLock()
	conn, ok := r.connection[0][key]
	r.lock[0].RUnlock()
	// 不存在不处理
	if !ok {
		return
	}
	// 检查状态
	conn.lock.Lock()
	switch conn.state {
	case connectingState:
		// 修改状态
		conn.state = closedState
	default: // 其他状态不处理
	}
	conn.lock.Unlock()
}

func (r *RUDP) handleCloseSegment(seg *segment) {

}

func (r *RUDP) handleInvalidSegment(seg *segment) {
	// 检查数据
	if seg.n != invalidSegmentLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(seg.b[invalidSegmentToken:])
	from := seg.b[0] >> 7
	r.lock[from].Lock()
	conn, ok := r.connected[from][token]
	r.lock[from].Unlock()
	if ok {
		conn.Close()
	}
}

func (r *RUDP) handleDataSegment(seg *segment) {
	// 检查数据
	if seg.n < dataSegmentPayload {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(seg.b[dataSegmentToken:])
	from := seg.b[0] >> 7
	r.lock[from].Lock()
	conn, ok := r.connected[from][token]
	r.lock[from].Unlock()
	if !ok {
		seg.b[segmentHeader] = (from << 7) | dataSegment
		binary.BigEndian.PutUint32(seg.b[invalidSegmentToken:], token)
		r.WriteTo(seg.b[:invalidSegmentLength], seg.a)
		return
	}
	// 添加数据
	sn := binary.BigEndian.Uint16(seg.b[dataSegmentSN:])
	conn.readLock.Lock()
	ok = conn.readFromUDP(sn, seg.b[dataSegmentPayload:seg.n])
	if ok {
		conn.encAckSegment(seg.b[:], sn)
		conn.readLock.Unlock()
		// 可读通知
		select {
		case conn.readable <- 1:
		default:
		}
		// 响应ack
		r.WriteToConn(seg.b[:ackSegmentLength], conn)
	} else {
		conn.readLock.Unlock()
	}
}

func (r *RUDP) handleAckSegment(seg *segment) {
	// 检查数据
	if seg.n != ackSegmentLength {
		return
	}
	// Conn
	token := binary.BigEndian.Uint32(seg.b[dataSegmentToken:])
	from := seg.b[0] >> 7
	r.lock[from].Lock()
	conn, ok := r.connected[from][token]
	r.lock[from].Unlock()
	if !ok {
		return
	}
	sn := binary.BigEndian.Uint32(seg.b[ackSegmentDataSN:])
	maxSn := binary.BigEndian.Uint32(seg.b[ackSegmentDataMaxSN:])
	free := binary.BigEndian.Uint32(seg.b[ackSegmentDataFree:])
	// ackSN := binary.BigEndian.Uint32(seg.b[ackSegmentSN:])
	conn.writeLock.Lock()
	if sn < maxSn {
		ok = conn.removeWriteDataBefore(maxSn, free)
	} else {
		ok = conn.removeWriteData(sn, free)
	}
	conn.writeLock.Unlock()
	if ok {
		// 可写通知
		select {
		case conn.writeable <- 1:
		default:
		}
	}
}

// 创建一个新的Conn变量
func (r *RUDP) newConn(segmentType, state byte, remoteAddr *net.UDPAddr) *Conn {
	c := new(Conn)
	c.segmentType = segmentType
	c.state = state
	c.quit = make(chan struct{})
	c.localListenAddr = r.conn.LocalAddr().(*net.UDPAddr)
	c.localInternetAddr = new(net.UDPAddr)
	c.remoteListenAddr = new(net.UDPAddr)
	c.remoteInternetAddr = remoteAddr
	c.readable = make(chan int, 1)
	c.writeable = make(chan int, 1)
	c.localEnableFEC = r.enableEFC
	c.localEnableCrypto = r.enableCrypto
	return c
}

// 创建一个新的客户端Conn
func (r *RUDP) newDialConn(address string) (*Conn, error) {
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	var key connectKey
	key.Init(addr)
	r.lock[0].Lock()
	// 连接耗尽
	if len(r.connection[0]) >= math.MaxInt32 {
		r.lock[0].Unlock()
		return nil, r.netOpError("dial", opErr("too many connections"))
	}
	conn := r.newConn(clientSegment, connectingState, addr)
	// 检查一个没有使用的token
	var ok bool
	for !r.closed {
		key.token = r.token[0]
		r.token[0]++
		_, ok = r.connection[0][key]
		if !ok {
			conn.cToken = key.token
			r.connection[0][key] = conn
			break
		}
	}
	r.lock[0].Unlock()
	// 探测mss
	conn.writeMSS = DetectMSS(addr)
	// 确定发送队列最大长度
	conn.writeQueueCap = calcQueueCap(writeBuffer, int(conn.writeMSS))
	return conn, nil
}

// 创建一个新的serverConn
func (r *RUDP) newAcceptConn(token uint32, addr *net.UDPAddr) (*Conn, bool) {
	var key connectKey
	key.token = token
	key.Init(addr)
	// 检查accepting Conn
	r.lock[1].Lock()
	conn, ok := r.connection[1][key]
	// 存在
	if ok {
		r.lock[1].Unlock()
		return conn, ok
	}
	// 连接耗尽
	if len(r.connection[1]) >= math.MaxInt32 {
		r.lock[1].Unlock()
		return nil, ok
	}
	conn = r.newConn(serverSegment, connectingState, addr)
	conn.cToken = token
	r.connection[1][key] = conn
	// 检查一个没有使用的token
	for !r.closed {
		key.token = r.token[1]
		r.token[1]++
		_, ok = r.connection[1][key]
		if !ok {
			conn.sToken = key.token
			r.connection[1][key] = conn
			break
		}
	}
	r.lock[1].Unlock()
	// 探测mss
	conn.writeMSS = DetectMSS(addr)
	// 确定发送队列最大长度
	conn.writeQueueCap = calcQueueCap(writeBuffer, int(conn.writeMSS))
	return conn, ok
}

// 关闭并释放Conn的资源
func (r *RUDP) freeConn(conn *Conn) {
	conn.lock.Lock()
	if conn.state == closedState {
		conn.lock.Unlock()
		return
	}
	conn.state = closedState
	conn.lock.Unlock()
	// 移除
	var key connectKey
	key.Init(conn.remoteInternetAddr)
	from := conn.segmentType >> 7
	key.token = conn.cToken
	r.lock[from].Lock()
	// connection
	delete(r.connection[from], key)
	// connected
	delete(r.connected[from], conn.sToken)
	r.lock[from].Unlock()
	// 释放资源
	close(conn.quit)
	close(conn.readable)
	close(conn.writeable)
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

func (r *RUDP) connRoutine(conn *Conn) {
	r.wait.Add(1)
	timer := time.NewTimer(conn.rto)
	defer func() {
		timer.Stop()
		r.freeConn(conn)
		r.wait.Done()
	}()
	for {
		select {
		case <-r.quit: // RUDP关闭信号
			return
		case <-conn.quit: // Conn关闭信号
			return
		case now := <-timer.C: // 超时重传检查
			conn.writeLock.Lock()
			conn.writeToUDP(func(data []byte) {
				r.WriteToConn(data, conn)
			}, now)
			conn.writeLock.Unlock()
			timer.Reset(conn.rto)
		}
	}
}
