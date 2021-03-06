package rudp

import (
	"net"
	"sync"
)

const (
	clientSegment = 1 << 7
	serverSegment = 0
)

// segment类型
const (
	dialSegment = iota
	acceptSegment
	rejectSegment
	dataSegment
	ackSegment
	closeSegment
	invalidSegment
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
	maxDataSN     = 0xffffff                        // dataSegement的最大sn，24位
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
	connectSegmentReadQueue  = connectSegmentMSS + 2        // 接收队列的空闲个数
	connectSegmentLength     = connectSegmentReadQueue + 1  // 长度
)

const (
	rejectSegmentVersion   = segmentToken + 4           // 协议版本，protolVersion
	rejectSegmentTimestamp = rejectSegmentVersion + 1   // 发起连接的时间戳
	rejectSegmentLength    = rejectSegmentTimestamp + 8 // 长度
)

const (
	dataSegmentSN      = segmentToken + 4  // sn
	dataSegmentPayload = dataSegmentSN + 3 // 数据起始
)

const (
	ackSegmentDataSN          = segmentToken + 4              // 数据包的sn
	ackSegmentDataMaxSN       = ackSegmentDataSN + 3          // 已收到数据包的最大sn
	ackSegmentReadQueueLength = ackSegmentDataMaxSN + 3       // 接收队列的空闲
	ackSegmentTimestamp       = ackSegmentReadQueueLength + 2 // ack的递增sn，sn更新才能更新ackSegmentReadQueueFree
	ackSegmentLength          = ackSegmentTimestamp + 8       // 长度
)

const (
	closeSegmentSN        = segmentToken + 4
	closeSegmentTimestamp = closeSegmentSN + 3
	closeSegmentLength    = closeSegmentTimestamp + 8
)

const (
	invalidSegmentTimestamp = segmentToken + 4
	invalidSegmentLength    = invalidSegmentTimestamp + 8
)

var (
	segmentPool sync.Pool
)

func init() {
	segmentPool.New = func() interface{} {
		return new(segment)
	}
}

// 一个udp数据包
type segment struct {
	b [maxMTU]byte
	n int
	a *net.UDPAddr
}

func (s *segment) String() string {
	switch s.b[0] {
	case dialSegment:
		return "dial segment"
	case acceptSegment:
		return "accept segment"
	case rejectSegment:
		return "reject segment"
	case dataSegment:
		return "data segment"
	case ackSegment:
		return "ack segment"
	case closeSegment:
		return "close segment"
	default:
		return "invalid segment"
	}
}

func uint24(b []byte) uint32 {
	return uint32(b[2]) | uint32(b[1])<<8 | uint32(b[0])<<16
}

func putUint24(b []byte, v uint32) {
	b[0] = byte(v >> 16)
	b[1] = byte(v >> 8)
	b[2] = byte(v)
}
