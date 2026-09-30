// Canonical conversion of caller values.

package keine

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
)

func TestCoerceBool(t *testing.T) {
	cases := []struct {
		in      any
		want    bool
		wantErr bool
	}{
		{true, true, false},
		{false, false, false},
		{int64(1), true, false},
		{int64(0), false, false},
		{"true", true, false},
		{"false", false, false},
		{"not a bool", false, true},
		{[]byte("true"), true, false},
		{[]byte("nope"), false, true},
		{nil, false, true},
		{struct{}{}, false, true},
	}
	for _, c := range cases {
		got, err := coerceBool(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("coerceBool(%T): want error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("coerceBool(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceBool(%T) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCoerceInt(t *testing.T) {
	ok := []struct {
		in   any
		want int64
	}{
		{int64(5), 5}, {int(5), 5}, {int8(5), 5}, {int16(5), 5}, {int32(5), 5},
		{uint(5), 5}, {uint8(5), 5}, {uint16(5), 5}, {uint32(5), 5}, {uint64(5), 5},
		{float32(5.9), 5}, {float64(5.9), 5},
		{true, 1}, {false, 0},
		{"42", 42}, {[]byte("42"), 42},
	}
	for _, c := range ok {
		got, err := coerceInt(c.in, 64)
		if err != nil {
			t.Errorf("coerceInt(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceInt(%T) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, in := range []any{"not a number", []byte("nope"), nil, struct{}{}} {
		if _, err := coerceInt(in, 64); err == nil {
			t.Errorf("coerceInt(%T): want error, got nil", in)
		}
	}
	if _, err := coerceInt("99999999999999999999", 64); err == nil {
		t.Error("coerceInt of an overflowing string: want error, got nil")
	}
}

func TestCoerceUint(t *testing.T) {
	ok := []struct {
		in   any
		want uint64
	}{
		{uint64(5), 5}, {uint(5), 5}, {uint8(5), 5}, {uint16(5), 5}, {uint32(5), 5},
		{int64(5), 5}, {int(5), 5}, {int8(5), 5}, {int16(5), 5}, {int32(5), 5},
		{float32(5.9), 5}, {float64(5.9), 5},
		{true, 1}, {false, 0},
		{"42", 42}, {[]byte("42"), 42},
	}
	for _, c := range ok {
		got, err := coerceUint(c.in, 64)
		if err != nil {
			t.Errorf("coerceUint(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceUint(%T) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, in := range []any{"-1", []byte("nope"), nil, struct{}{}} {
		if _, err := coerceUint(in, 64); err == nil {
			t.Errorf("coerceUint(%T): want error, got nil", in)
		}
	}
}

func TestCoerceFloat(t *testing.T) {
	ok := []struct {
		in   any
		want float64
	}{
		{float64(1.5), 1.5}, {float32(1.5), 1.5},
		{int64(2), 2}, {int(2), 2}, {int8(2), 2}, {int16(2), 2}, {int32(2), 2},
		{uint64(2), 2}, {uint(2), 2}, {uint8(2), 2}, {uint16(2), 2}, {uint32(2), 2},
		{true, 1}, {false, 0},
		{"1.5", 1.5}, {[]byte("1.5"), 1.5},
	}
	for _, c := range ok {
		got, err := coerceFloat(c.in, 64)
		if err != nil {
			t.Errorf("coerceFloat(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceFloat(%T) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, in := range []any{"not a number", []byte("nope"), nil, struct{}{}} {
		if _, err := coerceFloat(in, 64); err == nil {
			t.Errorf("coerceFloat(%T): want error, got nil", in)
		}
	}
}

func TestCoerceString(t *testing.T) {
	ok := []struct {
		in   any
		want string
	}{
		{"abc", "abc"}, {[]byte("abc"), "abc"},
		{int64(-5), "-5"}, {uint64(5), "5"}, {float64(5.5), "5.5"},
		{true, "true"}, {false, "false"},
	}
	for _, c := range ok {
		got, err := coerceString(c.in)
		if err != nil {
			t.Errorf("coerceString(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceString(%T) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, in := range []any{nil, struct{}{}} {
		if _, err := coerceString(in); err == nil {
			t.Errorf("coerceString(%T): want error, got nil", in)
		}
	}
}

func TestCoerceBytesAndParse(t *testing.T) {
	raw := []byte{1, 2, 3}
	got, err := coerceBytes(raw)
	if err != nil {
		t.Fatalf("coerceBytes: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("coerceBytes = %v, want %v", got, raw)
	}
	// The input slice must not be shared with the result.
	got[0] = 9
	if raw[0] != 1 {
		t.Errorf("coerceBytes returned a slice aliased with its input")
	}

	cases := []struct {
		in      string
		want    []byte
		wantErr bool
	}{
		{"[]", []byte{}, false},
		{"[1 2 3]", []byte{1, 2, 3}, false},
		{"garbage", nil, true},
		{"[999]", nil, true},
		{"[abc]", nil, true},
	}
	for _, c := range cases {
		got, err := parseBytesList(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseBytesList(%q): want error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBytesList(%q): %v", c.in, err)
			continue
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("parseBytesList(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, in := range []any{"garbage", nil, struct{}{}} {
		if _, err := coerceBytes(in); err == nil {
			t.Errorf("coerceBytes(%T): want error, got nil", in)
		}
	}
}

// canonicalValue coerces one value to the Go type typ implies. Only this test
// file asks for a single value; the writer and reader coerce whole columns.
func canonicalValue(v any, typ uint8) (any, error) {
	switch typ {
	case TypeBool:
		return coerceBool(v)
	case TypeInt8:
		n, err := coerceInt(v, 8)
		if err != nil {
			return nil, err
		}
		return int8(n), nil
	case TypeInt16:
		n, err := coerceInt(v, 16)
		if err != nil {
			return nil, err
		}
		return int16(n), nil
	case TypeInt32:
		n, err := coerceInt(v, 32)
		if err != nil {
			return nil, err
		}
		return int32(n), nil
	case TypeInt64:
		return coerceInt(v, 64)
	case TypeUint8:
		n, err := coerceUint(v, 8)
		if err != nil {
			return nil, err
		}
		return uint8(n), nil
	case TypeUint16:
		n, err := coerceUint(v, 16)
		if err != nil {
			return nil, err
		}
		return uint16(n), nil
	case TypeUint32:
		n, err := coerceUint(v, 32)
		if err != nil {
			return nil, err
		}
		return uint32(n), nil
	case TypeUint64:
		return coerceUint(v, 64)
	case TypeFloat32:
		f, err := coerceFloat(v, 32)
		if err != nil {
			return nil, err
		}
		return float32(f), nil
	case TypeFloat64:
		return coerceFloat(v, 64)
	case TypeString:
		return coerceString(v)
	case TypeBytes:
		return coerceBytes(v)
	default:
		return nil, fmt.Errorf("keine: unknown type %d", typ)
	}
}

func TestCanonicalValues(t *testing.T) {
	ok := []struct {
		v    any
		typ  uint8
		want any
	}{
		{true, TypeBool, true},
		{int64(-3), TypeInt8, int8(-3)},
		{int64(-3), TypeInt16, int16(-3)},
		{int64(-3), TypeInt32, int32(-3)},
		{int64(-3), TypeInt64, int64(-3)},
		{uint64(3), TypeUint8, uint8(3)},
		{uint64(3), TypeUint16, uint16(3)},
		{uint64(3), TypeUint32, uint32(3)},
		{uint64(3), TypeUint64, uint64(3)},
		{float64(1.5), TypeFloat32, float32(1.5)},
		{float64(1.5), TypeFloat64, float64(1.5)},
		{"abc", TypeString, "abc"},
		{[]byte{1, 2}, TypeBytes, []byte{1, 2}},
	}
	for _, c := range ok {
		got, err := canonicalValue(c.v, c.typ)
		if err != nil {
			t.Errorf("canonicalValue(%T, %d): %v", c.v, c.typ, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("canonicalValue(%T, %d) = %v, want %v", c.v, c.typ, got, c.want)
		}
	}

	for _, c := range []struct {
		v   any
		typ uint8
	}{
		{"300", TypeInt8},
		{"99999", TypeInt16},
		{"99999999999", TypeInt32},
		{"-1", TypeUint8},
		{"-1", TypeUint16},
		{"-1", TypeUint32},
		{"notafloat", TypeFloat32},
		{nil, TypeBool},
		{nil, TypeInt64},
		{nil, TypeUint64},
		{nil, TypeFloat64},
		{nil, TypeString},
		{nil, TypeBytes},
		{nil, 0xFF},
	} {
		if _, err := canonicalValue(c.v, c.typ); err == nil {
			t.Errorf("canonicalValue(%T, %d): want error, got nil", c.v, c.typ)
		}
	}
	if _, err := canonicalColumn([]any{"x"}, TypeInt8); err == nil {
		t.Error("canonicalColumn with an uncoercible value: want error, got nil")
	}
}
