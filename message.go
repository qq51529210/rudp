package rudp

import (
	"net"
	"sync"
)

// 消息类型定义
const (
	messageExtend                = iota // 可扩展的消息
	messagePing                         // 客户端
	messagePong                         // 服务端
	messageDial                         // 客户端创建连接
	messageAccept                       // 服务端接受连接
	messageRefuse                       // 服务端拒绝连接
	messageConnect                      // 客户端确认收到accept
	messageCloseC                       // 客户端关闭连接
	messageCloseS                       // 服务端关闭连接
	messageDataC                        // 客户端发送未加密数据
	messageDataS                        // 服务端发送未加密数据
	messageCryptoDataC                  // 客户端发送已加密数据
	messageCryptoDataS                  // 服务端发送已加密数据
	messageCryptoDataCIncomplete        // 客户端发送已加密数据，但数据量小于密钥长度
	messageCryptoDataSIncomplete        // 服务端发送未加密数据，但数据量小于密钥长度
	messageAckC                         // 客户端发送数据包确认接收
	messageAckS                         // 服务端发送数据包确认接收
	messageAliveC                       // 客户端发送心跳
	messageAliveS                       // 服务端发送心跳
	messageReqInfoC                     // 客户端发送状态查询
	messageReqInfoS                     // 服务端发送状态查询
	messageResInfoC                     // 客户端发送状态查询响应
	messageResInfoS                     // 服务端发送状态查询响应
)

const (
	fromClient = 0x00
	fromServer = 0x80
)

// 消息结构下标定义
const (
	messageIndex   = 0
	MsgCTokenIndex = messageIndex + 1
	MsgSTokenIndex = MsgCTokenIndex + 4
	MsgMinLen      = MsgSTokenIndex + 4

	MsgPingIdIndex   = messageIndex + 1
	MsgPingDataIndex = MsgPingIdIndex + 8
	MsgPingMinLen    = MsgPingDataIndex

	MsgPongIdIndex            = messageIndex + 1
	MsgPongVersionIndex       = MsgPongIdIndex + 8
	MsgPongInternetIPIndex    = MsgPongVersionIndex + 8
	MsgPongInternetPortIndex  = MsgPongInternetIPIndex + 16
	MsgPongMsgPingLengthIndex = MsgPongInternetPortIndex + 2
	MsgPongLen                = MsgPongMsgPingLengthIndex + 2

	MsgDialInternetIPIndex        = MsgCTokenIndex + 4
	MsgDialInternetPortIndex      = MsgDialInternetIPIndex + 16
	MsgDialListenIPIndex          = MsgDialInternetPortIndex + 2
	MsgDialListenPortIndex        = MsgDialListenIPIndex + 16
	MsgDialVersionIndex           = MsgDialListenPortIndex + 2
	MsgDialDialTimeoutIndex       = MsgDialVersionIndex + 8
	MsgDialConnectionTimeoutIndex = MsgDialDialTimeoutIndex + 8
	MsgDialMSSIndex               = MsgDialConnectionTimeoutIndex + 8
	MsgDialReadQueueIndex         = MsgDialMSSIndex + 2
	MsgDialWriteQueueIndex        = MsgDialReadQueueIndex + 4
	MsgDialCryptoIndex            = MsgDialWriteQueueIndex + 4
	MsgDialLen                    = MsgDialCryptoIndex + 1

	MsgAcceptInternetIPIndex        = MsgSTokenIndex + 4
	MsgAcceptInternetPortIndex      = MsgAcceptInternetIPIndex + 16
	MsgAcceptListenIPIndex          = MsgAcceptInternetPortIndex + 2
	MsgAcceptListenPortIndex        = MsgAcceptListenIPIndex + 16
	MsgAcceptVersionIndex           = MsgAcceptListenPortIndex + 2
	MsgAcceptConnectionTimeoutIndex = MsgAcceptVersionIndex + 8
	MsgAcceptMSSIndex               = MsgAcceptConnectionTimeoutIndex + 8
	MsgAcceptReadQueueIndex         = MsgAcceptMSSIndex + 2
	MsgAcceptWriteQueueIndex        = MsgAcceptReadQueueIndex + 4
	MsgAcceptCryptoIndex            = MsgAcceptWriteQueueIndex + 4
	MsgAcceptLen                    = MsgAcceptCryptoIndex + 1

	MsgRefuseVersionIndex = MsgCTokenIndex + 4
	MsgRefuseLen          = MsgRefuseVersionIndex + 8

	MsgConnectLen = MsgSTokenIndex + 4

	MsgCloseTimestampIndex    = MsgSTokenIndex + 4
	MsgCloseStateIndex        = MsgCloseTimestampIndex + 8
	MsgCloseReadPacketIndex   = MsgCloseStateIndex + 1
	MsgCloseWritePacketIndex  = MsgCloseReadPacketIndex + 8
	MsgCloseReadFlowIndex     = MsgCloseWritePacketIndex + 8
	MsgCloseWriteFlowIndex    = MsgCloseReadFlowIndex + 8
	MsgCloseMinRTTIndex       = MsgCloseWriteFlowIndex + 8
	MsgCloseMaxRTTIndex       = MsgCloseMinRTTIndex + 8
	MsgCloseMaxBindWidthIndex = MsgCloseMaxRTTIndex + 8
	MsgCloseLen               = MsgCloseMaxBindWidthIndex + 8

	MsgDataSNIndex  = MsgSTokenIndex + 4
	MsgDataRawIndex = MsgDataSNIndex + 4
	MsgDataMinLen   = MsgDataRawIndex

	MsgDataIncompleteLenIndex = MsgDataSNIndex + 4
	MsgDataIncompleteRawIndex = MsgDataIncompleteLenIndex + 1

	MsgAckSNIndex     = MsgSTokenIndex + 4
	MsgAckMaxSNIndex  = MsgAckSNIndex + 4
	MsgAckRemainIndex = MsgAckMaxSNIndex + 4
	MsgAckLen         = MsgAckRemainIndex + 4

	MsgAliveTimestampIndex   = MsgSTokenIndex + 4
	MsgAliveRTOIndex         = MsgAliveTimestampIndex + 8
	MsgAliveReadPacketIndex  = MsgAliveRTOIndex + 8
	MsgAliveWritePacketIndex = MsgAliveReadPacketIndex + 8
	MsgAliveReadFlowIndex    = MsgAliveWritePacketIndex + 8
	MsgAliveWriteFlowIndex   = MsgAliveReadFlowIndex + 8
	MsgAliveLen              = MsgAliveWriteFlowIndex + 8

	MsgReqInfoSNIndex = MsgSTokenIndex + 4
	MsgReqInfoLen     = MsgReqInfoSNIndex + 4

	MsgResInfoSNIndex     = MsgSTokenIndex + 4
	MsgResInfoMaxSNIndex  = MsgResInfoSNIndex + 4
	MsgResInfoRemainIndex = MsgResInfoMaxSNIndex + 4
	MsgResInfoLen         = MsgResInfoRemainIndex + 4
)

var _messagePool sync.Pool

func init() {
	_messagePool.New = func() interface{} {
		return new(message)
	}
}

type message struct {
	i int          // 序号
	b [MaxMSS]byte // 数据缓存
	n int          // 数据大小
	a *net.UDPAddr // 对方的地址
}

func (this *message) Bytes() []byte {
	return this.b[:this.n]
}

func (this *message) EncPing() {
	this.b[0] = messagePing
}

func (this *message) EncDial() {
	this.b[0] = messageDial
}
