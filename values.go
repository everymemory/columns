// Conversion between a typed slice and []any. The interfaces boxValues builds
// point at values where they already sit, so a wide column costs one allocation
// instead of one per value.

package keine

import "unsafe"

// boxValues converts a typed slice, []int64, []string and so on, into []any
// without copying a value. Each interface header points at its element where it
// already sits in the source slice, so a column costs one allocation instead of
// one per value.
//
// Interior pointers into s are safe because the decoders build a fresh slice for
// every column and the caller takes ownership of it, so nothing reuses that
// backing array. A type the format never uses has no values to box, and the
// decoders only ever pass one of the thirteen.
func boxValues(typed any) []any {
	switch s := typed.(type) {
	case []bool:
		return boxSlice(s)
	case []int8:
		return boxSlice(s)
	case []int16:
		return boxSlice(s)
	case []int32:
		return boxSlice(s)
	case []int64:
		return boxSlice(s)
	case []uint8:
		return boxSlice(s)
	case []uint16:
		return boxSlice(s)
	case []uint32:
		return boxSlice(s)
	case []uint64:
		return boxSlice(s)
	case []float32:
		return boxSlice(s)
	case []float64:
		return boxSlice(s)
	case []string:
		return boxSlice(s)
	case [][]byte:
		return boxSlice(s)
	}
	return nil
}

// boxSlice is boxValues for one element type.
func boxSlice[T any](s []T) []any {
	out := make([]any, len(s))
	if len(s) == 0 {
		return out
	}

	// Converting the zero value once yields the type pointer every interface in
	// the column then shares. Converting a real element would give the same
	// pointer and cost the same single allocation.
	var zero T
	sample := any(zero)
	tag := (*efaceHeader)(unsafe.Pointer(&sample)).typ

	hdrs := unsafe.Slice((*efaceHeader)(unsafe.Pointer(unsafe.SliceData(out))), len(out))
	for i := range s {
		hdrs[i].typ = tag
		hdrs[i].data = unsafe.Pointer(&s[i])
	}
	return out
}

// efaceHeader is the layout of an empty interface, which is what a []any holds.
// It is the empty counterpart of runtime.iface, with a type pointer in place of
// an itab.
type efaceHeader struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

// asValues converts a typed slice into []T. The second result is false when the
// slice's element type is not T, so a wrong type is an error rather than a
// silent mismatch.
func asValues[T any](typed any) ([]T, bool) {
	switch s := typed.(type) {
	case []bool:
		return convertSlice[bool, T](s)
	case []int8:
		return convertSlice[int8, T](s)
	case []int16:
		return convertSlice[int16, T](s)
	case []int32:
		return convertSlice[int32, T](s)
	case []int64:
		return convertSlice[int64, T](s)
	case []uint8:
		return convertSlice[uint8, T](s)
	case []uint16:
		return convertSlice[uint16, T](s)
	case []uint32:
		return convertSlice[uint32, T](s)
	case []uint64:
		return convertSlice[uint64, T](s)
	case []float32:
		return convertSlice[float32, T](s)
	case []float64:
		return convertSlice[float64, T](s)
	case []string:
		return convertSlice[string, T](s)
	case [][]byte:
		return convertSlice[[]byte, T](s)
	}
	return nil, false
}

func convertSlice[S any, T any](s []S) ([]T, bool) {
	// A slice holds one element type, so the zero value settles whether T fits
	// without checking each value.
	var zero S
	if _, ok := any(zero).(T); !ok {
		return nil, false
	}
	out := make([]T, len(s))
	for i, v := range s {
		out[i] = any(v).(T)
	}
	return out, true
}
