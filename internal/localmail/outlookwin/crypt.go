package outlookwin

// 数据块的两种编码（[MS-PST] 5.1、5.2）。它们是混淆而不是加密，没有密钥：
// 置换编码查一张固定表，循环编码的「密钥」就是块自己的 BID。

// decodePermute 解 NDB_CRYPT_PERMUTE：每个字节查 mpbbI（表的第三段）。
func decodePermute(b []byte) {
	t := mpbbCrypt[512:768]
	for i, c := range b {
		b[i] = t[c]
	}
}

// encodePermute 是 decodePermute 的逆（查 mpbbR），只给测试造夹具用。
func encodePermute(b []byte) {
	t := mpbbCrypt[0:256]
	for i, c := range b {
		b[i] = t[c]
	}
}

// cryptCyclicBuf 是 NDB_CRYPT_CYCLIC，对称：编码解码同一个函数。key 是 BID 的低 32 位。
func cryptCyclicBuf(b []byte, key uint32) {
	r, s, in := mpbbCrypt[0:256], mpbbCrypt[256:512], mpbbCrypt[512:768]
	w := uint16(key ^ (key >> 16))
	for i, c := range b {
		c += byte(w)
		c = r[c]
		c += byte(w >> 8)
		c = s[c]
		c -= byte(w >> 8)
		c = in[c]
		c -= byte(w)
		b[i] = c
		w++
	}
}
