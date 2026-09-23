package keine

// EncodeBitmap packs a null mask: bit i is set when row i is null, least
// significant bit first, padded to a byte boundary.
func EncodeBitmap(nulls []bool) []byte {
	b := make([]byte, (len(nulls)+7)/8)
	for i, v := range nulls {
		if v {
			b[i/8] |= 1 << (i % 8)
		}
	}
	return b
}

// DecodeBitmap unpacks n bits from the result of EncodeBitmap.
func DecodeBitmap(b []byte, n int) []bool {
	nulls := make([]bool, n)
	for i := 0; i < n; i++ {
		if i/8 >= len(b) {
			break
		}
		if b[i/8]&(1<<(i%8)) != 0 {
			nulls[i] = true
		}
	}
	return nulls
}
