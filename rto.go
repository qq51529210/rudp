package rudp

import "time"

const (
	minRTO = 10 * time.Millisecond  // 最小超时重传，毫秒
	maxRTO = 100 * time.Millisecond // 最大超时重传，毫秒
)

type rto struct {
	rto time.Duration // 超时重发
	rtt time.Duration // 实时RTT，用于计算rto
	avr time.Duration // 平均RTT，用于计算rto
	min time.Duration // 最小rto，防止发送"过快"
	max time.Duration // 最大rto，防止发送"假死"
}

// 网上抄的tcp计算的方法
func (rto *rto) Calculate(rtt time.Duration) {
	rto.avr = (3*rto.avr + rto.rtt - rtt) / 4
	rto.rtt = (7*rto.rtt + rtt) / 8
	rto.rto = rto.rtt + 4*rto.avr
	if rto.min != 0 && rto.rto < rto.min {
		rto.rto = rto.min
	}
	if rto.max != 0 && rto.rto > rto.max {
		rto.rto = rto.max
	}
}
