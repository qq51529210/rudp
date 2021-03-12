package rudp

import (
	"bytes"
	"testing"
)

func Test_DiffieHellman(t *testing.T) {
	dh1 := newDiffieHellman()
	dh2 := newDiffieHellman()
	exchangeKey1 := make([]byte, 32)
	exchangeKey2 := make([]byte, 32)
	// 生成交换密钥
	dh1.ExchangeKey(exchangeKey1)
	dh2.ExchangeKey(exchangeKey2)
	// 生成加密密钥
	cryptoKey1 := make([]byte, 32)
	cryptoKey2 := make([]byte, 32)
	dh1.CryptoKey(exchangeKey2, cryptoKey1)
	dh2.CryptoKey(exchangeKey1, cryptoKey2)
	// 比较
	if bytes.Compare(cryptoKey1, cryptoKey2) != 0 {
		t.FailNow()
	}
}
