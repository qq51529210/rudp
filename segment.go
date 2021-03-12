package rudp

import (
	"net"
	"sync"
)

// segment类型
const (
	dialSegment = iota
	acceptSegment
	handshakeSuccessSegment
	pingSegment
	pongSegment
	clientDataSegment
	serverDataSegment
	clientAckSegment
	serverAckSegment
	clientDiscardSegment
	serverDiscardSegment
	clientDiscardAckCmdSegment
	serverDiscardAckCmdSegment
	invalidSegment
	serverInvalidSegment
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
	segmentType = 0
)

// dialSegment & acceptSegment
const (
	handshakeSegmentVersion     = segmentType + 1                  // 协议版本，protolVersion
	handshakeSegmentToken       = handshakeSegmentVersion + 1      // 连接的随机token
	handshakeSegmentTimestamp   = handshakeSegmentToken + 4        // 发起连接的时间戳
	handshakeSegmentLocalIP     = handshakeSegmentTimestamp + 8    // local listen ip
	handshakeSegmentLocalPort   = handshakeSegmentLocalIP + 16     // local listen port
	handshakeSegmentRemoteIP    = handshakeSegmentLocalPort + 2    // remote internet ip
	handshakeSegmentRemotePort  = handshakeSegmentRemoteIP + 16    // remote internet port
	handshakeSegmentMSS         = handshakeSegmentRemotePort + 2   // 探测的mss，用于对方调整接收队列
	handshakeSegmentTimeout     = handshakeSegmentMSS + 2          // client dial timeout，由于server会一直发送accept segment
	handshakeSegmentFEC         = handshakeSegmentTimeout + 8      // 是否在后面的数据传输中启用纠错
	handshakeSegmentCrypto      = handshakeSegmentFEC + 1          // 是否在后面的数据传输中启用加密
	handshakeSegmentExchangeKey = handshakeSegmentCrypto + 1       // 如果启用加密，Diffie-Hellman交换密钥的随机数
	handshakeSegmentLength      = handshakeSegmentExchangeKey + 32 // 长度
)

const (
	handshakeSuccessSegmentToken  = segmentType + 1
	handshakeSuccessSegmentLength = handshakeSuccessSegmentToken + 4
)

const (
	dataSegmentToken   = segmentType + 1
	dataSegmentSN      = dataSegmentToken + 4
	dataSegmentPayload = dataSegmentSN + 2
)

const (
	ackSegmentToken     = segmentType + 1
	ackSegmentDataSN    = ackSegmentToken + 4
	ackSegmentDataMaxSN = ackSegmentDataSN + 2
	ackSegmentDataFree  = ackSegmentDataMaxSN + 2
	ackSegmentSN        = ackSegmentDataFree + 2
	ackSegmentLength    = ackSegmentSN + 4
)

const (
	discardSegmentToken  = segmentType + 1
	discardSegmentSN     = discardSegmentToken + 4
	discardSegmentBegin  = discardSegmentToken + 2
	discardSegmentEnd    = discardSegmentBegin + 2
	discardSegmentLength = discardSegmentEnd + 2
)

const (
	discardAckSegmentToken  = segmentType + 1
	discardAckSegmentSN     = discardAckSegmentToken + 4
	discardAckSegmentLength = discardAckSegmentSN + 2
)

const (
	invalidSegmentToken  = segmentType + 1
	invalidSegmentLength = invalidSegmentToken + 4
)

// 一个udp数据包
type segment struct {
	b [maxMTU]byte
	n int
	a *net.UDPAddr
}

var (
	segmentPool          sync.Pool
	dataSegment          = []byte{clientDataSegment, serverDataSegment}
	ackSegment           = []byte{clientAckSegment, serverAckSegment}
	discardSegment       = []byte{clientDiscardSegment, serverDiscardSegment}
	discardAckCmdSegment = []byte{clientDiscardAckCmdSegment, serverDiscardAckCmdSegment}
)

func init() {
	segmentPool.New = func() interface{} {
		return new(segment)
	}
}
