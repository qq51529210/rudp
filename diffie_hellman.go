package rudp

import "math/big"

var (
	diffieHellmanP = big.NewInt(0) // Diffie-Hellman交换密钥算法的质数
	diffieHellmanG = big.NewInt(0) // Diffie-Hellman交换密钥算法的质数
)

func init() {
	diffieHellmanP.SetBytes([]byte("RUDP DIFFIE-HELLMAN KEY EXCHANGE")) // 正好32个
	diffieHellmanG.SetBytes([]byte("rudp diffie-hellman key exchange")) // 正好32个
}

// 1.生成本地的随机数
// 2.生成exchange key
// 3.交换exchange key
// 4.根据得到的exchange key，生成crypto key
type diffieHellman struct {
	number *big.Int
}

// 生成本地的随机数
func newDiffieHellman() *diffieHellman {
	dh := new(diffieHellman)
	var buff [32]byte
	mathRand.Read(buff[:])
	dh.number = big.NewInt(0).SetBytes(buff[:])
	return dh
}

// 生成exchange key，
func (dh *diffieHellman) ExchangeKey(key []byte) {
	exchangeNumber := big.NewInt(0).Exp(diffieHellmanG, dh.number, diffieHellmanP)
	exchangeNumber.FillBytes(key[:])
}

// 根据exchange key，生成crypto key
func (dh *diffieHellman) CryptoKey(exchangeKey, cryptoKey []byte) {
	// 根据exchange key生成数字
	exchangeNumber := big.NewInt(0).SetBytes(exchangeKey)
	// 生成crypto key
	cryptoNumber := big.NewInt(0).Exp(exchangeNumber, dh.number, diffieHellmanP)
	cryptoNumber.FillBytes(cryptoKey[:])
}
