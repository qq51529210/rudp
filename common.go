package rudp

import (
	"errors"
	"math/rand"
	"net"
	"time"
)

var (
	mathRand        = rand.New(rand.NewSource(time.Now().Unix())) // 随机数
	DetectMSS       = detectMSS                                   // 返回mss和rtt
	connClosedError = errors.New("conn has been closed")          // conn被关闭
	rudpClosedError = errors.New("rudp has been closed")          // rudp被关闭
)

// net.timeout的接口
type timeoutError struct{}

func (e timeoutError) Error() string { return "timeout" }
func (e timeoutError) Timeout() bool { return true }

// 探测链路的mss，返回最适合的mss，以免被分包
func detectMSS(*net.UDPAddr) (uint16, error) {
	return maxMSS, nil
}
