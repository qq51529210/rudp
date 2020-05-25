package rudp
//
//import (
//	"bytes"
//	"crypto/md5"
//	"encoding/binary"
//	"sync"
//	"testing"
//	"time"
//)
//
//// 模拟数据传输完整性
//func Test_Buffer(t *testing.T) {
//	wh := md5.New()
//	rh := md5.New()
//	rb := newReadBuffer(maxMSS, defaultReadBuffer)
//	wb := newWriteBuffer(maxMSS, defaultWriteBuffer)
//	wait := sync.WaitGroup{}
//	quit := make(chan struct{})
//	// 发送数据次数
//	count := 100
//	// 延迟
//	delay := time.Millisecond * 1
//	// 丢包率
//	loss := 10
//	wait.Add(3)
//	// 发送方
//	go func() {
//		defer wait.Done()
//		buf := make([]byte, 1024)
//		for i := 0; i < count; i++ {
//			// 随机数据
//			_rand.Read(buf)
//			// 哈希发送的数据
//			wh.Write(buf)
//			// 写入缓存
//			p := buf
//			for len(p) > 0 {
//				wb.Lock()
//				n := wb.Write(0, p)
//				wb.Unlock()
//				if n < 1 {
//					<-wb.enable
//					continue
//				}
//				p = p[n:]
//			}
//		}
//		wb.Lock()
//		wb.WriteEOF(0)
//		wb.Unlock()
//	}()
//	// 接收方
//	go func() { // 读数据
//		defer wait.Done()
//		buf := make([]byte, 1024)
//		for {
//			// 从缓存读数据
//			rb.Lock()
//			n := rb.Read(buf)
//			rb.Unlock()
//			if n < 0 {
//				close(quit)
//				return
//			}
//			if n < 1 {
//				<-rb.enable
//				continue
//			}
//			// 哈希接收到的数据
//			rh.Write(buf[:n])
//		}
//	}()
//	// 模拟网络
//	go func() { // 传输数据
//		timer := time.NewTimer(0)
//		defer func() {
//			timer.Stop()
//			wait.Done()
//		}()
//		rb.bytes++
//		wb.bytes++
//		for {
//			select {
//			case <-quit:
//				return
//			case now := <-timer.C:
//				var datas [][]byte
//				// 超时发送
//				wb.Lock()
//				wb.WriteToUDP(func(data []byte) {
//					// 模拟丢包率
//					if _rand.Intn(100) >= loss {
//						b := make([]byte, len(data))
//						copy(b, data)
//						datas = append(datas, b)
//					}
//				}, now)
//				wb.Unlock()
//				// 网络传输延迟
//				time.Sleep(delay)
//				// 接收
//				for i := 0; i < len(datas); i++ {
//					// 模拟丢包率
//					if _rand.Intn(100) >= loss {
//						sn := binary.BigEndian.Uint32(datas[i][msgSN:])
//						rb.Lock()
//						ok := rb.ReadFromUDP(sn, datas[i][msgPayload:])
//						if ok {
//							rb.Enable()
//						}
//						rb.Unlock()
//						// 模拟丢包率
//						if ok && _rand.Intn(100) >= loss {
//							wb.Lock()
//							wb.Remove(sn)
//							wb.Enable()
//							wb.Unlock()
//						}
//					}
//				}
//				timer.Reset(wb.rto)
//			}
//		}
//	}()
//	//
//	wait.Wait()
//	//
//	if !bytes.Equal(wh.Sum(nil), rh.Sum(nil)) {
//		t.FailNow()
//	}
//	//
//	rb.Release()
//	wb.Release()
//}
