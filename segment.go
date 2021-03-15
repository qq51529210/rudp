package rudp

import (
	"net"
	"sync"
)

// segment类型
const (
	dialSegment = iota
	acceptSegment
	connectedSegment
	pingSegment
	pongSegment
	invalidSegment
	dataSegment
	dataAckSegment
	discardSegment
	discardAckSegment
	// clientDataSegment
	// serverDataSegment
	// clientAckSegment
	// serverAckSegment
	// clientDiscardSegment
	// serverDiscardSegment
	// clientDiscardAckCmdSegment
	// serverDiscardAckCmdSegment
	// invalidSegment
	// serverInvalidSegment
)

const (
	protolVersion = 1                               // rudp协议版本
	ipV4Header    = 20                              // IPv4数据包头大小
	ipV6Header    = 40                              // IPv6数据包头大小
	udpHeader     = 8                               // UDP数据包头大小
	minMTU        = 576                             // 链路最小的MTU
	maxMTU        = 1500                            // 链路最大的MTU
	minMSS        = minMTU - ipV6Header - udpHeader // 应用层最小数据包
	maxMSS        = maxMTU - ipV4Header - udpHeader // 应用层最大数据包
)

const (
	segmentType  = 0               // segment类型
	segmentToken = segmentType + 1 // 发起连接的随机token
)

const (
	connectSegmentVersion    = segmentToken + 4             // 协议版本，protolVersion
	connectSegmentTimestamp  = connectSegmentVersion + 1    // 发起连接的时间戳
	connectSegmentLocalIP    = connectSegmentTimestamp + 8  // local listen ip
	connectSegmentLocalPort  = connectSegmentLocalIP + 16   // local listen port
	connectSegmentRemoteIP   = connectSegmentLocalPort + 2  // remote internet ip
	connectSegmentRemotePort = connectSegmentRemoteIP + 16  // remote internet port
	connectSegmentMSS        = connectSegmentRemotePort + 2 // 探测的mss，用于对方调整接收队列
	connectSegmentTimeout    = connectSegmentMSS + 2        // client dial timeout，server发送acceptSegment超时判断
	connectSegmentLength     = connectSegmentTimeout + 8    // 长度
)

const (
	connectedSegmentLength = segmentToken + 4
)

const (
	pingSegmentSN     = segmentToken + 4
	pingSegmentLength = pingSegmentSN + 4
)

const (
	pongSegmentSN     = segmentToken + 4
	pongSegmentLength = pongSegmentSN + 4
)

const (
	dataSegmentSN      = segmentToken + 4
	dataSegmentPayload = dataSegmentSN + 2
)

const (
	dataAckSegmentDataSN    = segmentToken + 4            // 数据包的sn
	dataAckSegmentDataMaxSN = dataAckSegmentDataSN + 2    // 已收到数据包的最大sn
	dataAckSegmentFree      = dataAckSegmentDataMaxSN + 2 // 接收队列的空闲
	dataAckSegmentSN        = dataAckSegmentFree + 2
	dataAckSegmentLength    = dataAckSegmentSN + 4
)

const (
	discardSegmentSN     = segmentToken + 4
	discardSegmentBegin  = discardSegmentSN + 2
	discardSegmentEnd    = discardSegmentBegin + 2
	discardSegmentLength = discardSegmentEnd + 2
)

const (
	discardAckSegmentSN     = segmentToken + 4
	discardAckSegmentLength = discardAckSegmentSN + 2
)

const (
	invalidSegmentLength = segmentToken + 4
)

// 一个udp数据包
type segment struct {
	b [maxMTU]byte
	n int
	a *net.UDPAddr
}

var (
	segmentPool sync.Pool
	// dataSegment          = []byte{clientDataSegment, serverDataSegment}
	// ackSegment           = []byte{clientAckSegment, serverAckSegment}
	// discardSegment       = []byte{clientDiscardSegment, serverDiscardSegment}
	// discardAckCmdSegment = []byte{clientDiscardAckCmdSegment, serverDiscardAckCmdSegment}
	checkSegment = []func(seg *segment) bool{
		func(seg *segment) bool {
			return seg.b[connectSegmentVersion] == protolVersion && seg.n == connectSegmentLength
		}, // dialSegment
		func(seg *segment) bool {
			return seg.b[connectSegmentVersion] == protolVersion && seg.n == connectSegmentLength
		}, // acceptSegment
		func(seg *segment) bool {
			return seg.b[connectSegmentVersion] == protolVersion && seg.n == connectedSegmentLength
		}, // connectedSegment
		func(seg *segment) bool {
			return seg.n == pingSegmentLength
		}, // pingSegment
		func(seg *segment) bool {
			return seg.n == pongSegmentLength
		}, // pongSegment
		func(seg *segment) bool {
			return seg.n == invalidSegmentLength
		}, // invalidSegment
		func(seg *segment) bool {
			return seg.n >= dataSegmentPayload
		}, // dataSegment
		func(seg *segment) bool {
			return seg.n == dataAckSegmentLength
		}, // dataAckSegment
		func(seg *segment) bool {
			return seg.n == discardSegmentLength
		}, // discardSegment
		func(seg *segment) bool {
			return seg.n == discardAckSegmentLength
		}, // discardAckSegment
	}
)

func init() {
	segmentPool.New = func() interface{} {
		return new(segment)
	}
}
