package rudp

import (
	"net"
	"sync"
)

// 消息类型定义
const (
	msgExtend  = iota // 可扩展的消息
	msgDial           // c->s，请求创建连接，握手1
	msgAccept         // s->c，接受连接，握手2
	msgRefuse         // s->c，拒绝连接，握手2
	msgConnect        // c->s，收到接受连接的消息，握手3
	msgData           // p<->p，数据
	msgAck            // p<->p，收到数据确认
	msgPing           // c->s
	msgPong           // s->c
)

const (
	ipV4Header = 20                              // IPv4数据包头大小
	ipV6Header = 40                              // IPv6数据包头大小
	udpHeader  = 8                               // UDP数据包头大小
	minMTU     = 576                             // 链路最小的MTU
	maxMTU     = 1500                            // 链路最大的MTU
	minMSS     = minMTU - ipV6Header - udpHeader // 应用层最小数据包
	maxMSS     = maxMTU - ipV4Header - udpHeader // 应用层最大数据包
	msgVersion = 1                               // 消息协议的版本
	msgBuff    = maxMSS                          // 消息缓存的最大容量
)

var _msgPool sync.Pool

func init() {
	_msgPool.New = func() interface{} {
		return new(message)
	}
}

type message struct {
	b [msgBuff]byte // 数据缓存
	n int           // 数据大小
	a *net.UDPAddr  // 对方的地址
}

func (this *message) Type() byte {
	return this.b[0]
}

func (this *message) Bytes() []byte {
	return this.b[:this.n]
}

//type msgInfo struct {
//	_type   byte         // ping/pong
//	version uint32       // 版本号
//	addr    *net.UDPAddr // 客户端的监听地址
//}
//
//func (this *message) EncInfo(msg *msgInfo) {
//	// type 1
//	this.b[0] = msg._type
//	// version 4
//	binary.BigEndian.PutUint32(this.b[1:], msg.version)
//	// address
//	this.EncAddress(msg.addr, 5)
//	this.n = 23
//}
//
//func (this *message) DecInfo(msg *msgInfo) {
//	// version 4
//	msg.version = binary.BigEndian.Uint32(this.b[1:])
//	// address
//	msg.addr = new(net.UDPAddr)
//	this.DecAddress(msg.addr, 5)
//}
//
//type msgDial struct {
//	version uint32       // 版本号
//	token   uint32       // 客户端的随机token
//	lAddr   *net.UDPAddr // 客户端的监听地址
//	maxMSS  uint16       // 客户端到服务端链路的检测的最大包
//	rBuf    uint32       // 客户端的接收缓存
//	wBuf    uint32       // 客户端的发送缓存
//	crypto  bool         // 客户端的是否启用了加密
//}

const (
	msgDialVersion      = 1                        // 版本号
	msgDialToken        = msgDialVersion + 4       // 客户端的随机token
	msgDialLocalAddress = msgDialToken + 4         // 客户端的监听地址
	msgDialMaxMSS       = msgDialLocalAddress + 18 // 客户端到服务端链路的检测的最大包
	msgDialReadBuffer   = msgDialMaxMSS + 2        // 客户端的接收缓存
	msgDialWriteBuffer  = msgDialReadBuffer + 4    // 客户端的发送缓存
	msgDialCrypto       = msgDialWriteBuffer + 4   // 客户端的是否启用了加密
	msgDialLength       = msgDialCrypto + 1        // 消息长度
)

//func (this *message) EncDial(msg *msgDial) {
//	// type 1
//	this.b[0] = msgTypeDial
//	// version 4
//	binary.BigEndian.PutUint32(this.b[msgDialVersion:], msg.version)
//	// client token 4
//	binary.BigEndian.PutUint32(this.b[msgDialToken:], msg.token)
//	// address
//	this.EncAddress(msg.lAddr, msgDialLocalAddress)
//	// mss 2
//	binary.BigEndian.PutUint16(this.b[msgDialMaxMSS:], msg.maxMSS)
//	// read buffer 4
//	binary.BigEndian.PutUint32(this.b[msgDialReadBuffer:], msg.rBuf)
//	// write buffer 4
//	binary.BigEndian.PutUint32(this.b[msgDialWriteBuffer:], msg.wBuf)
//	// crypto 1
//	if msg.crypto {
//		this.b[msgDialCrypto] = 1
//	} else {
//		this.b[msgDialCrypto] = 0
//	}
//	this.n = msgDialLength
//}
//
//func (this *message) DecDial(msg *msgDial) bool {
//	if this.n != msgDialLength {
//		return false
//	}
//	// version 4
//	msg.version = binary.BigEndian.Uint32(this.b[msgDialVersion:])
//	// client token 4
//	msg.token = binary.BigEndian.Uint32(this.b[msgDialToken:])
//	// address
//	msg.lAddr = new(net.UDPAddr)
//	this.DecAddress(msg.lAddr, msgDialLocalAddress)
//	// mss 2
//	msg.maxMSS = binary.BigEndian.Uint16(this.b[msgDialMaxMSS:])
//	// read buffer 4
//	msg.rBuf = binary.BigEndian.Uint32(this.b[msgDialReadBuffer:])
//	// write buffer 4
//	msg.wBuf = binary.BigEndian.Uint32(this.b[msgDialWriteBuffer:])
//	// crypto 1
//	msg.crypto = this.b[msgDialCrypto] == 1
//	return true
//}
//
//type msgAccept struct {
//	version uint32       // 版本号
//	cToken  uint32       // 客户端的请求附带的token
//	sToken  uint32       // 服务端的产生的的token
//	cAddr   *net.UDPAddr // 客户端的公网地址
//	maxMSS  uint16       // 服务端到客户端链路的检测的最大包
//	rBuf    uint32       // 服务端的接收缓存
//	wBuf    uint32       // 服务端的发送缓存
//	crypto  bool         // 服务端的是否启用了加密
//}
//
//const (
//	msgAcceptVersion       = 1
//	msgAcceptClientToken   = msgAcceptVersion + 4
//	msgAcceptServerToken   = msgAcceptClientToken + 4
//	msgAcceptClientAddress = msgAcceptServerToken + 4
//	msgAcceptMaxMSS        = msgAcceptClientAddress + 18
//	msgAcceptReadBuffer    = msgAcceptMaxMSS + 2
//	msgAcceptWriteBuffer   = msgAcceptReadBuffer + 4
//	msgAcceptCrypto        = msgAcceptWriteBuffer + 4
//	msgAcceptLength        = msgAcceptCrypto + 1
//)
//
//func (this *message) EncAccept(msg *msgAccept) {
//	// type 1
//	this.b[0] = msgTypeAccept
//	// version 4
//	binary.BigEndian.PutUint32(this.b[msgAcceptVersion:], msg.version)
//	// client token 4
//	binary.BigEndian.PutUint32(this.b[msgAcceptClientToken:], msg.cToken)
//	// server token 4
//	binary.BigEndian.PutUint32(this.b[msgAcceptServerToken:], msg.sToken)
//	// client internet address
//	this.EncAddress(msg.cAddr, msgAcceptClientAddress)
//	// mss 2
//	binary.BigEndian.PutUint16(this.b[msgAcceptMaxMSS:], msg.maxMSS)
//	// read buffer 4
//	binary.BigEndian.PutUint32(this.b[msgAcceptReadBuffer:], msg.rBuf)
//	// write buffer 4
//	binary.BigEndian.PutUint32(this.b[msgAcceptWriteBuffer:], msg.wBuf)
//	// crypto 1
//	if msg.crypto {
//		this.b[msgAcceptCrypto] = 1
//	} else {
//		this.b[msgAcceptLength] = 0
//	}
//	this.n = msgAcceptLength
//}
//
//func (this *message) DecAccept(msg *msgAccept) bool {
//	if this.n != msgAcceptLength {
//		return false
//	}
//	// version 4
//	msg.version = binary.BigEndian.Uint32(this.b[msgAcceptVersion:])
//	// client token 4
//	msg.cToken = binary.BigEndian.Uint32(this.b[msgAcceptClientToken:])
//	// server token 4
//	msg.sToken = binary.BigEndian.Uint32(this.b[msgAcceptServerToken:])
//	// address
//	msg.cAddr = new(net.UDPAddr)
//	this.DecAddress(msg.cAddr, msgAcceptClientAddress)
//	// mss 2
//	msg.maxMSS = binary.BigEndian.Uint16(this.b[msgAcceptMaxMSS:])
//	// read buffer 4
//	msg.rBuf = binary.BigEndian.Uint32(this.b[msgAcceptReadBuffer:])
//	// write buffer 4
//	msg.wBuf = binary.BigEndian.Uint32(this.b[msgAcceptWriteBuffer:])
//	// crypto 1
//	msg.crypto = this.b[msgAcceptCrypto] == 1
//	return true
//}
//
//type msgRefuse struct {
//	version uint32 // 版本号
//	token   uint32 // 客户端的请求附带的token
//	reason  uint32 // 拒绝的原因
//}
//
//const (
//	msgRefuseVersion = 1
//	msgRefuseToken   = msgRefuseVersion + 4
//	msgRefuseReason  = msgRefuseToken + 4
//	msgRefuseLength  = msgRefuseReason + 1
//)
//
//func (this *message) EncRefuse(msg *msgRefuse) {
//	// type 1
//	this.b[0] = msgTypeRefuse
//	// version 4
//	binary.BigEndian.PutUint32(this.b[msgRefuseVersion:], msg.version)
//	// client token 4
//	binary.BigEndian.PutUint32(this.b[msgRefuseToken:], msg.token)
//	// reason 4
//	binary.BigEndian.PutUint32(this.b[msgRefuseReason:], msg.reason)
//	this.n = msgRefuseLength
//}
//
//func (this *message) DecRefuse(msg *msgRefuse) bool {
//	if this.n != msgRefuseLength {
//		return false
//	}
//	// version 4
//	msg.version = binary.BigEndian.Uint32(this.b[msgRefuseVersion:])
//	// client token 4
//	msg.token = binary.BigEndian.Uint32(this.b[msgRefuseToken:])
//	// reason 4
//	msg.reason = binary.BigEndian.Uint32(this.b[msgRefuseReason:])
//	return true
//}
//
//type msgConnect struct {
//	token uint32 // 客户端的随机token
//}
//
//const (
//	msgConnectVersion = 1
//	msgConnectToken   = msgConnectVersion + 4
//	msgConnectLength  = msgConnectToken + 1
//)
//
//func (this *message) EncConnect(msg *msgConnect) {
//	// type 1
//	this.b[0] = msgTypeConnect
//	// client token 4
//	binary.BigEndian.PutUint32(this.b[msgConnectToken:], msg.token)
//	this.n = msgConnectLength
//}
//
//func (this *message) DecConnect(msg *msgConnect) bool {
//	if this.n != msgConnectLength {
//		return false
//	}
//	// client token 4
//	msg.token = binary.BigEndian.Uint32(this.b[msgConnectToken:])
//	return true
//}
//
//type msgData struct {
//	token uint64 // connection token
//	sn    uint32 // 收到连续的数据序号
//	data  []byte // 数据
//}
//
//const (
//	msgDataToken   = 1
//	msgDataSN      = msgDataToken + 8
//	msgDataPayload = msgDataSN + 4
//)
//
//func (this *message) EncData(msg *msgData) {
//	// type 1
//	this.b[0] = msgTypeData
//	// token 8
//	binary.BigEndian.PutUint64(this.b[msgDataToken:], msg.token)
//	// sn 4
//	binary.BigEndian.PutUint32(this.b[msgDataSN:], msg.sn)
//	// data
//	this.n = msgDataPayload + copy(this.b[msgDataPayload:], msg.data)
//}
//
//func (this *message) DecData(msg *msgData) bool {
//	if this.n <= msgDataPayload {
//		return false
//	}
//	// token 8
//	msg.token = binary.BigEndian.Uint64(this.b[msgDataToken:])
//	// sn 4
//	msg.sn = binary.BigEndian.Uint32(this.b[msgDataSN:])
//	// data
//	msg.data = this.b[msgDataPayload:this.n]
//	return true
//}
//
//type msgACK struct {
//	token uint64 // connection token
//	sn    uint32 // 收到连续的数据序号
//	maxSN uint32 // 收到最大的数据序号
//	free  uint32 // 接受缓存，空闲的数据大小
//}
//
//const (
//	msgACKToken  = 1
//	msgACKSN     = msgACKToken + 8
//	msgACKMaxSN  = msgACKSN + 4
//	msgACKFree   = msgACKMaxSN + 4
//	msgACKLength = msgACKFree + 4
//)
//
//func (this *message) EncACK(msg *msgACK) {
//	// type 1
//	this.b[0] = msgTypeACK
//	// token 8
//	binary.BigEndian.PutUint64(this.b[msgACKToken:], msg.token)
//	// sn 4
//	binary.BigEndian.PutUint32(this.b[msgACKSN:], msg.sn)
//	// max sn 4
//	binary.BigEndian.PutUint32(this.b[msgACKMaxSN:], msg.maxSN)
//	// free 4
//	binary.BigEndian.PutUint32(this.b[msgACKFree:], msg.free)
//	this.n = msgACKLength
//}
//
//func (this *message) DecACK(msg *msgACK) bool {
//	if this.n != msgACKLength {
//		return false
//	}
//	// token 48
//	msg.token = binary.BigEndian.Uint64(this.b[msgACKToken:])
//	// sn 4
//	msg.sn = binary.BigEndian.Uint32(this.b[msgACKSN:])
//	// max sn 4
//	msg.maxSN = binary.BigEndian.Uint32(this.b[msgACKMaxSN:])
//	// free 4
//	msg.free = binary.BigEndian.Uint32(this.b[msgACKFree:])
//	return true
//}
//
//type msgPingPong struct {
//	_type byte
//	token uint64 // connection token
//}
//
//const (
//	msgPingPongToken  = 1
//	msgPingPongLength = msgPingPongToken + 8
//)
//
//func (this *message) EncPingPong(msg *msgPingPong) {
//	// type 1
//	this.b[0] = msg._type
//	// token 8
//	binary.BigEndian.PutUint64(this.b[msgPingPongToken:], msg.token)
//	this.n = msgPingPongLength
//}
//
//func (this *message) DecPingPong(msg *msgPingPong) bool {
//	if this.n != msgPingPongLength {
//		return false
//	}
//	// token 8
//	msg.token = binary.BigEndian.Uint64(this.b[msgPingPongToken:])
//	return true
//}

// 处理消息MsgDial
func (this *RUDP) handleMsgDial(msg *message) {

}

// 处理消息MsgAccept
func (this *RUDP) handleMsgAccept(msg *message) {

}

// 处理消息MsgRefuse
func (this *RUDP) handleMsgRefuse(msg *message) {

}

// 处理消息MsgConnect
func (this *RUDP) handleMsgConnect(msg *message) {

}

// 处理消息MsgData
func (this *RUDP) handleMsgData(msg *message) {

}

// 处理消息MsgAck
func (this *RUDP) handleMsgAck(msg *message) {

}

// 处理消息MsgPing
func (this *RUDP) handleMsgPing(msg *message) {

}

// 处理消息MsgPong
func (this *RUDP) handleMsgPong(msg *message) {

}
