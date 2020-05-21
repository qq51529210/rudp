package rudp

import (
	"encoding/binary"
	"math/rand"
	"net"
	"time"
)

// 如果n1==0，返回n2
func defaultInt(n1, n2 int) int {
	if n1 == 0 {
		return n2
	}
	return n1
}

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

// 读写字节
type uint64RW struct {
	r uint64
	w uint64
}

// 读写字节
type uint32RW struct {
	r uint32
	w uint32
}

// 新建RUDP的参数
type Config struct {
	Listen          string `json:"listen"`            // udp监听地址，不能为空
	UDPDataRoutine  int    `json:"udp_data_routine"`  // udp原始数据包的处理协程个数，默认是runtime.NumCPU()
	AcceptQueue     int    `json:"accept_queue"`      // 作为服务端时，接受连接的队列。为0则不作为server，Accept()函数返回错误
	ConnReadBuffer  int    `json:"conn_read_buffer"`  // conn的读缓存大小，用于窗口控制，默认是defaultReadBuffer
	ConnWriteBuffer int    `json:"conn_write_buffer"` // conn的写缓存大小，用于窗口控制，默认是defaultWriteBuffer
	ConnRTO         int    `json:"conn_rto"`          // conn的默认rto，msgDial/msgAccept消息超时重发，单位ms，默认100ms
	ConnMinRTO      int    `json:"conn_min_rto"`      // conn的最小rto，人为控制不要出现发送"过快"，单位ms，0表示不控制
	ConnMaxRTO      int    `json:"conn_max_rto"`      // conn的最大rto，人为控制不要出现发送"过慢"，单位ms，0表示不控制
}

// ipV4地址转ipV6整数时用
const v4InV6Prefix = uint64(0xff)<<40 | uint64(0xff)<<32

// ip地址作为map连接池的键
type keyAddr struct {
	ip1  uint64 // IPV6地址字符数组前64位
	ip2  uint64 // IPV6地址字符数组后64位
	port uint16 // 端口
}

func (this *keyAddr) Init(addr *net.UDPAddr) {
	if len(addr.IP) == net.IPv4len {
		this.ip1 = 0
		this.ip2 = v4InV6Prefix |
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

// 连接池的键，因为客户端可能在nat后面，所以加上token
type connKey struct {
	keyAddr
	//cToken uint16
	//sToken uint16
	token uint32
}

const (
	clientConn = 0
	serverConn = 1
)

// 一些默认值
const (
	defaultReadBuffer  = 1024 * 40 // Conn默认的读缓存大小
	defaultWriteBuffer = 1024 * 40 // Conn默认的写缓存大小
	defaultConnectRTO  = 100      // 客户端/服务端，建立连接，默认的发送超时，毫秒
)

const (
	maxConn = 1<<32 - 1 // 32位整数最大值
)

// 随机数
var _rand = rand.New(rand.NewSource(time.Now().Unix()))

// 为了避免分包，应该探测本地到对方的udp数据包最合适的mss
// 这里返回最小值，可以重写这个函数来确定最大值
var DetectMSS = func(*net.UDPAddr) uint16 {
	return minMSS
}

// 计算队列最大长度
func calcMaxLen(size uint32, mss uint16) uint32 {
	// 计算数据块队列的长度
	if size < uint32(mss) {
		return 1
	}
	// 长度=字节/mss
	max := size / uint32(mss)
	// 不能整除，+1
	if size%uint32(mss) != 0 {
		max++
	}
	return max
}

type opErr string

func (e opErr) Error() string   { return string(e) }
func (e opErr) Timeout() bool   { return string(e) == "timeout" }
func (e opErr) Temporary() bool { return false }

type closeErr string

func (e closeErr) Error() string   { return string(e) + " has been closed" }
func (e closeErr) Timeout() bool   { return false }
func (e closeErr) Temporary() bool { return false }

type timeoutErr string

func (e timeoutErr) Error() string   { return string(e) + "timeout" }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return false }
