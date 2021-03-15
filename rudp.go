package rudp

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"time"
)

var (
	readQueueCap    = uint16(0xffff / 2)                          // 默认的读缓存队列最大长度
	writeQueueCap   = uint16(0xffff / 2)                          // 默认的写缓冲队列最大长度
	handleQueueLen  = uint16(1024)                                // 默认的conn的处理队列长度
	connectRTO      = 100 * time.Millisecond                      // connect segment超时重传，毫秒
	mathRand        = rand.New(rand.NewSource(time.Now().Unix())) // 随机数
	DetectMSS       = detectMSS                                   // 返回mss和rtt
	connClosedError = errors.New("conn has been closed")          // conn被关闭
	rudpClosedError = errors.New("rudp has been closed")          // rudp被关闭
	minRTO          = 10 * time.Millisecond                       // 最小超时重传，毫秒
	maxRTO          = 100 * time.Millisecond                      // 最大超时重传，毫秒
)

// 连接池的键，因为客户端可能在nat后面，所以加上token
type connKey struct {
	ip1   uint64 // IPV6地址字符数组前64位
	ip2   uint64 // IPV6地址字符数组后64位
	port  uint16 // 端口
	token uint32 // token
}

// 将128位的ip地址（v4的转成v6）的字节分成两个64位整数，加上端口，作为key
func (this *connKey) Init(a *net.UDPAddr) {
	if len(a.IP) == net.IPv4len {
		this.ip1 = 0
		this.ip2 = uint64(0xff)<<40 | uint64(0xff)<<32 |
			uint64(a.IP[0])<<24 | uint64(a.IP[1])<<16 |
			uint64(a.IP[2])<<8 | uint64(a.IP[3])
	} else {
		this.ip1 = binary.BigEndian.Uint64(a.IP[0:])
		this.ip2 = binary.BigEndian.Uint64(a.IP[8:])
	}
	this.port = uint16(a.Port)
}

// 探测链路的mss，返回最适合的mss，以免被分包
func detectMSS(*net.UDPAddr) (uint16, error) {
	return maxMSS, nil
}

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
	connReadQueueCap  uint16               // 新连接的接收队列大小
	connWriteQueueCap uint16               // 新连接的发送队列大小
}

// 设置新的连接是否启用纠错
func (r *RUDP) EnableFEC(fec byte) {
	r.fec = fec
}

// 设置新的连接是否启用加密
func (r *RUDP) EnableCrypto(crypto byte) {
	r.crypto = crypto
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
	ticker := time.NewTicker(r.connectRTO)
	defer ticker.Stop()
	// 初始化dial connect segment
	var buff [maxMSS]byte
	r.encConnectSegment(conn, buff[:], dialSegment, timeout)
	for err == nil {
		select {
		case <-time.After(timeout): // 连接超时
			err = new(timeoutError)
		case <-ticker.C: // 超时重传
			r.WriteToConn(buff[:connectSegmentLength], conn)
		case <-conn.connectSignal: // 连接信号
			switch conn.state {
			case connectState: // 已连接，返回
				r.waitGroup.Add(2)
				go r.clientConnRoutine(conn)
				go r.connHandleSegmentRoutine(conn)
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
	r.removeConn(conn)
	return nil, conn.netOpError("dial", err)
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

// 读取并处理udp数据包
func (r *RUDP) handleUDPDataRoutine() {
	r.waitGroup.Add(1)
	defer r.waitGroup.Done()
	var err error
	var from byte
	var conn *Conn
	var ok bool
	key := new(connKey)
	for !r.closed {
		seg := segmentPool.Get().(*segment)
		// 读取数据并处理
		seg.n, seg.a, err = r.conn.ReadFromUDP(seg.b[:])
		if err != nil {
			// todo: 日志
			continue
		}
		r.ioBytes[0] += uint64(seg.n)
		// 处理
		if !checkSegment[seg.b[0]](seg) {
			segmentPool.Put(seg)
			continue
		}
		key.Init(seg.a)
		key.token = binary.BigEndian.Uint32(seg.b[segmentToken:])
		switch seg.b[0] {
		case dialSegment:
			timestamp := binary.BigEndian.Uint64(seg.b[connectSegmentTimestamp:])
			// 获取Conn
			r.lock[1].Lock()
			conn, ok = r.connection[1][*key]
			if !ok {
				// 不存在，注册到connection
				conn = new(Conn)
				conn.timestamp = timestamp
				r.connection[1][*key] = conn
				r.lock[1].Unlock()
				// 启动routine
				r.waitGroup.Add(1)
				go r.serverConnRoutine(conn, seg, key.token)
			} else {
				if timestamp > conn.timestamp {
					// 有相应的Conn，但是timestamp比较新，说明是新的dialSegment
					r.waitGroup.Add(1)
					// 在routine中关闭
					go func(c *Conn) {
						defer r.waitGroup.Done()
						r.removeConn(c)
					}(conn)
					// 新的Conn
					conn = new(Conn)
					conn.timestamp = timestamp
					r.connection[1][*key] = conn
					r.lock[1].Unlock()
					// 启动routine
					r.waitGroup.Add(1)
					go r.serverConnRoutine(conn, seg, key.token)
				} else {
					r.lock[1].Unlock()
					segmentPool.Put(seg)
				}
			}
		case invalidSegment:
			from = seg.b[0] >> 7
			r.lock[from].RLock()
			conn, ok = r.connection[from][*key]
			r.lock[from].RUnlock()
			if !ok {
				segmentPool.Put(seg)
			} else {
				// 在routine中关闭
				go func(c *Conn) {
					defer r.waitGroup.Done()
					r.removeConn(c)
				}(conn)
			}
		default:
			from = seg.b[0] >> 7
			if from == 1 {
				r.lock[0].RLock()
				conn, ok = r.connection[0][*key]
				r.lock[0].RUnlock()
			} else {
				r.lock[1].RLock()
				conn, ok = r.connection[1][*key]
				r.lock[1].RUnlock()
			}
			if !ok {
				// 不存在，响应invalidSegment
				r.writeInvalidSegment(seg, invalidSegment, key.token)
				segmentPool.Put(seg)
			} else {
				// 添加到Conn处理队列
				select {
				case conn.handleQueue <- seg:
				default:
					segmentPool.Put(seg)
				}
			}
		}
	}
}

func (r *RUDP) writeConnectedSegment(seg *segment, token uint32) {
	seg.b[segmentType] = connectedSegment
	binary.BigEndian.PutUint32(seg.b[segmentToken:], token)
	r.WriteTo(seg.b[:connectedSegmentLength], seg.a)
}

func (r *RUDP) writeInvalidSegment(seg *segment, segType byte, token uint32) {
	seg.b[segmentType] = segType
	binary.BigEndian.PutUint32(seg.b[segmentToken:], token)
	r.WriteTo(seg.b[:invalidSegmentLength], seg.a)
}

func (r *RUDP) encConnectSegment(conn *Conn, buff []byte, segType byte, timeout time.Duration) {
	buff[segmentType] = segType
	buff[connectSegmentVersion] = protolVersion
	binary.BigEndian.PutUint32(buff[segmentToken:], conn.token)
	binary.BigEndian.PutUint64(buff[connectSegmentTimestamp:], conn.timestamp)
	copy(buff[connectSegmentLocalIP:], conn.listenAddr[0].IP.To16())
	binary.BigEndian.PutUint16(buff[connectSegmentLocalPort:], uint16(conn.listenAddr[0].Port))
	copy(buff[connectSegmentRemoteIP:], conn.internetAddr[1].IP.To16())
	binary.BigEndian.PutUint16(buff[connectSegmentRemotePort:], uint16(conn.internetAddr[1].Port))
	binary.BigEndian.PutUint16(buff[connectSegmentMSS:], conn.mss[0])
	binary.BigEndian.PutUint64(buff[connectSegmentTimeout:], uint64(timeout))
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
			r.lock[0].Unlock()
			return nil, r.netOpError("dial", rudpClosedError)
		}
		_, ok = r.connection[0][key]
		if !ok {
			conn = new(Conn)
			r.connection[0][key] = conn
			r.lock[0].Unlock()
			break
		}
		key.token++
		if key.token == token {
			r.lock[0].Unlock()
			return nil, r.netOpError("dial", fmt.Errorf("too many connections to %s", addr.String()))
		}
	}
	// 初始化Conn
	r.initConn(conn, addr, 0, dialState, key.token, mss)
	conn.timestamp = uint64(time.Now().Unix())
	return conn, nil
}

func (r *RUDP) initConn(conn *Conn, addr *net.UDPAddr, from, state byte, token uint32, mss uint16) {
	conn.rudp = r
	conn.from = from
	conn.token = token
	conn.state = state
	conn.quitSignal = make(chan struct{})
	conn.connectSignal = make(chan struct{})
	conn.handleQueue = make(chan *segment, r.connHandleQueue)
	conn.listenAddr[0] = r.conn.LocalAddr().(*net.UDPAddr)
	conn.listenAddr[1] = new(net.UDPAddr)
	conn.internetAddr[0] = new(net.UDPAddr)
	conn.internetAddr[1] = addr
	conn.ioEnable[0] = make(chan byte, 1)
	conn.ioEnable[1] = make(chan byte, 1)
	conn.dataCap[0] = r.connReadQueueCap
	conn.dataCap[1] = r.connWriteQueueCap
	conn.minRTO = minRTO
	conn.maxRTO = maxRTO
	conn.mss[0] = mss
}

func (r *RUDP) removeConn(conn *Conn) {
	// 修改状态
	conn.lock.Lock()
	if conn.state == closedState {
		conn.lock.Unlock()
		return
	}
	conn.state = closedState
	conn.lock.Unlock()
	// 释放资源
	close(conn.quitSignal)
	close(conn.ioEnable[0])
	close(conn.ioEnable[1])
	conn.releaseReadData()
	conn.releaseWriteData()
	for seg := range conn.handleQueue {
		segmentPool.Put(seg)
	}
	// 移除
	var key connKey
	key.Init(conn.internetAddr[1])
	key.token = conn.token
	r.lock[conn.from].Lock()
	delete(r.connection[conn.from], key)
	r.lock[conn.from].Unlock()
}

func (r *RUDP) serverConnRoutine(conn *Conn, seg *segment, token uint32) {
	defer func() {
		segmentPool.Put(seg)
		r.removeConn(conn)
		r.waitGroup.Done()
	}()
	// 探测mss
	mss, err := detectMSS(seg.a)
	if err != nil {
		// todo log
		return
	}
	// 初始化Conn
	r.initConn(conn, seg.a, 1, dialState, token, mss)
	// 读取dialSegment字段
	copy(conn.listenAddr[1].IP, seg.b[connectSegmentLocalIP:])
	conn.listenAddr[1].Port = int(binary.BigEndian.Uint16(seg.b[connectSegmentLocalPort:]))
	copy(conn.internetAddr[0].IP, seg.b[connectSegmentLocalIP:])
	conn.internetAddr[0].Port = int(binary.BigEndian.Uint16(seg.b[connectSegmentRemotePort:]))
	conn.mss[1] = binary.BigEndian.Uint16(seg.b[connectSegmentMSS:])
	// 计时器
	timer := time.NewTimer(r.connectRTO)
	defer timer.Stop()
	// 先发送accept
	timeout := time.Duration(binary.BigEndian.Uint64(seg.b[connectSegmentTimeout:]))
	var buff [maxMSS]byte
	r.encConnectSegment(conn, buff[:], acceptSegment, timeout)
Loop:
	for {
		select {
		case <-time.After(timeout): // 连接超时
			return
		case <-timer.C: // 超时重传
			r.WriteToConn(buff[:connectSegmentLength], conn)
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
	timer.Reset(conn.rto)
	for {
		select {
		case <-r.quitSignal: // RUDP关闭信号
			return
		case <-conn.quitSignal: // Conn关闭信号
			return
		case now := <-timer.C: // 超时重传检查
			conn.retransmission(&now)
			timer.Reset(conn.rto)
		}
	}
}

func (r *RUDP) clientConnRoutine(conn *Conn) {
	timer := time.NewTimer(conn.rto)
	defer func() {
		timer.Stop()
		r.removeConn(conn)
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
			timer.Reset(conn.rto)
		}
	}
}

func (r *RUDP) connHandleSegmentRoutine(conn *Conn) {
	defer r.waitGroup.Done()
	for {
		seg, ok := <-conn.handleQueue
		if !ok {
			return
		}
		switch seg.b[0] {
		case acceptSegment:
			conn.lock.Lock()
			switch conn.state {
			case dialState:
				// 状态
				conn.state = connectState
				conn.lock.Unlock()
				// 发送信号
				close(conn.connectSignal)
				// 响应connectedSegment
				r.writeConnectedSegment(seg, conn.token)
			case connectState:
				conn.lock.Unlock()
				// 已经连接，响应connectedSegment
				r.writeConnectedSegment(seg, conn.token)
			default:
				conn.lock.Unlock()
				// 状态不对，响应invalidSegment
				r.writeInvalidSegment(seg, invalidSegment, conn.token)
			}
		case connectedSegment:
			conn.lock.Lock()
			switch conn.state {
			case dialState:
				// 修改状态
				conn.state = connectState
				conn.lock.Unlock()
				// 发送连接信号
				close(conn.connectSignal)
			case connectedSegment:
				conn.lock.Unlock()
			default:
				conn.lock.Unlock()
				// 状态不对，响应invalidSegment
				r.writeInvalidSegment(seg, invalidSegment, conn.token)
			}
		case pingSegment:
			seg.b[segmentType] = pongSegment
			r.WriteToConn(seg.b[:pongSegmentLength], conn)
		case pongSegment:
			sn := binary.BigEndian.Uint32(seg.b[pongSegmentSN:])
			conn.dataLock[1].Lock()
			if sn == conn.pingSN {
				conn.pingSN++
				conn.readTime = time.Now()
			}
			conn.dataLock[1].Unlock()
		case dataSegment:
			sn := binary.BigEndian.Uint16(seg.b[dataSegmentSN:])
			data := seg.b[dataSegmentPayload:seg.n]
			// 尝试添加
			var ok bool
			conn.dataLock[0].Lock()
			ok = conn.addReadData(sn, data)
			conn.dataLock[0].Unlock()
			if ok {
				// 响应dataAckSegment
				seg.b[segmentType] = dataSegment | conn.from
				binary.BigEndian.PutUint32(seg.b[segmentToken:], conn.token)
				binary.BigEndian.PutUint16(seg.b[dataAckSegmentDataSN:], sn)
				binary.BigEndian.PutUint16(seg.b[dataAckSegmentDataMaxSN:], conn.sn[0])
				binary.BigEndian.PutUint16(seg.b[dataAckSegmentFree:], conn.dataCap[0]-conn.dataLen[0])
				r.WriteToConn(seg.b[:dataAckSegmentLength], conn)
			}
		case dataAckSegment:
			sn := binary.BigEndian.Uint16(seg.b[dataAckSegmentDataSN:])
			maxSN := binary.BigEndian.Uint16(seg.b[dataAckSegmentDataMaxSN:])
			// 移除
			ok := false
			conn.dataLock[1].Lock()
			if maxSN > sn {
				ok = conn.removeWriteDataBefore(maxSN)
			} else {
				ok = conn.removeWriteData(sn)
			}
			conn.dataLock[1].Unlock()
			// 更新
			if ok {
				id := binary.BigEndian.Uint32(seg.b[dataAckSegmentSN:])
				if id > conn.ackSN[1] {
					conn.writeDataMax = binary.BigEndian.Uint16(seg.b[dataAckSegmentFree:])
				}
			}
		case discardSegment:
			sn := binary.BigEndian.Uint16(seg.b[discardSegmentSN:])
			begin := binary.BigEndian.Uint16(seg.b[discardSegmentBegin:])
			end := binary.BigEndian.Uint16(seg.b[discardSegmentEnd:])
			// 移除
			conn.discardReadData(sn, begin, end)
			// 响应discardAckSegment
			seg.b[segmentType] = discardAckSegment | conn.from
			binary.BigEndian.PutUint32(seg.b[segmentToken:], conn.token)
			binary.BigEndian.PutUint16(seg.b[discardAckSegmentSN:], sn)
			r.WriteToConn(seg.b[:discardAckSegmentLength], conn)
		case discardAckSegment:
			sn := binary.BigEndian.Uint16(seg.b[discardAckSegmentSN:])
			// 更新write discard sn
			conn.dataLock[1].Lock()
			if conn.discardSN[1] == sn {
				conn.discardSN[1]++
			}
			conn.dataLock[1].Unlock()
		}
		segmentPool.Put(seg)
	}
}
