package columns

import "reflect"

// Type tags identify the logical type of a column's values. Strings share the
// variable length encodings used for byte slices.
const (
	TypeBool    uint8 = 0x01
	TypeInt8    uint8 = 0x02
	TypeInt16   uint8 = 0x03
	TypeInt32   uint8 = 0x04
	TypeInt64   uint8 = 0x05
	TypeUint8   uint8 = 0x06
	TypeUint16  uint8 = 0x07
	TypeUint32  uint8 = 0x08
	TypeUint64  uint8 = 0x09
	TypeFloat32 uint8 = 0x0A
	TypeFloat64 uint8 = 0x0B
	TypeBytes   uint8 = 0x0C
	TypeString  uint8 = 0x0D
)

// goType is the slice type a column of typ is stored as, which is what the
// encoders consume and the statistics walk. AddRowGroupTyped checks its input
// against it rather than coercing, so a caller handing the writer typed columns
// cannot store an int32 column under an int64 schema.
var goType = map[uint8]reflect.Type{
	TypeBool:    reflect.TypeOf([]bool(nil)),
	TypeInt8:    reflect.TypeOf([]int8(nil)),
	TypeInt16:   reflect.TypeOf([]int16(nil)),
	TypeInt32:   reflect.TypeOf([]int32(nil)),
	TypeInt64:   reflect.TypeOf([]int64(nil)),
	TypeUint8:   reflect.TypeOf([]uint8(nil)),
	TypeUint16:  reflect.TypeOf([]uint16(nil)),
	TypeUint32:  reflect.TypeOf([]uint32(nil)),
	TypeUint64:  reflect.TypeOf([]uint64(nil)),
	TypeFloat32: reflect.TypeOf([]float32(nil)),
	TypeFloat64: reflect.TypeOf([]float64(nil)),
	TypeBytes:   reflect.TypeOf([][]byte(nil)),
	TypeString:  reflect.TypeOf([]string(nil)),
}

// typedColumn reports whether col is a slice of the Go type typ implies. An
// unknown type tag is not one any schema should carry, and it reports a
// mismatch the way canonicalColumnTyped reports an error.
func typedColumn(typ uint8, col any) bool {
	want, ok := goType[typ]
	return ok && reflect.TypeOf(col) == want
}

// Encoding tags identify how a column chunk's values were laid out.
const (
	EncPlain       uint8 = 0x01
	EncRLEBitpack  uint8 = 0x02
	EncDelta       uint8 = 0x03
	EncDict        uint8 = 0x04
	EncOffsetBytes uint8 = 0x05
	EncAffix       uint8 = 0x06
	EncTokenized   uint8 = 0x07
)

// Compress tags identify the compression codec applied to a chunk's encoded
// data. CompressZstd is unimplemented and reports an error.
const (
	CompressNone  uint8 = 0x00
	CompressZstd  uint8 = 0x01
	CompressFlate uint8 = 0x02
	CompressGzip  uint8 = 0x03
	CompressZlib  uint8 = 0x04
	CompressLzw   uint8 = 0x05
)
