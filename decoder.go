package dbus

import (
	"encoding/binary"
	"io"
	"reflect"
)

type decoder struct {
	in    io.Reader
	order binary.ByteOrder
	pos   int
	fds   []int

	// The following fields are used to reduce memory allocs.
	buf []byte
	d   float64
	t   uint64
	x   int64
	u   uint32
	i   int32
	n   int16
	q   uint16
	y   [1]byte
}

// newDecoder returns a new decoder that reads values from in. The input is
// expected to be in the given byte order.
func newDecoder(in io.Reader, order binary.ByteOrder, fds []int) *decoder {
	dec := new(decoder)
	dec.in = in
	dec.order = order
	dec.fds = fds
	return dec
}

// Reset resets the decoder to be reading from in.
func (dec *decoder) Reset(in io.Reader, order binary.ByteOrder, fds []int) {
	dec.in = in
	dec.order = order
	dec.pos = 0
	dec.fds = fds
}

// align aligns the input to the given boundary and panics on error.
func (dec *decoder) align(n int) {
	if dec.pos%n != 0 {
		newpos := (dec.pos + n - 1) & ^(n - 1)
		if err := dec.read2buf(newpos - dec.pos); err != nil {
			panic(err)
		}
		dec.pos = newpos
	}
}

// Calls binary.Read(dec.in, dec.order, v) and panics on read errors.
func (dec *decoder) binread(v interface{}) {
	if err := binary.Read(dec.in, dec.order, v); err != nil {
		panic(err)
	}
}

func (dec *decoder) Decode(sig Signature) (vs []interface{}, err error) {
	defer func() {
		var ok bool
		v := recover()
		if err, ok = v.(error); ok {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				err = FormatError("unexpected EOF")
			}
		}
	}()
	vs = make([]interface{}, 0)
	s := sig.str
	for s != "" {
		err, rem := validSingle(s, &depthCounter{})
		if err != nil {
			return nil, err
		}
		v := dec.decode(s[:len(s)-len(rem)], 0)
		vs = append(vs, v)
		s = rem
	}
	return vs, nil
}

// read2buf reads exactly n bytes from the reader dec.in into the buffer dec.buf
// to reduce memory allocs.
// The buffer grows automatically.
func (dec *decoder) read2buf(n int) error {
	if cap(dec.buf) < n {
		dec.buf = make([]byte, n)
	} else {
		dec.buf = dec.buf[:n]
	}
	_, err := io.ReadFull(dec.in, dec.buf)
	return err
}

// decodeU decodes uint32 obtained from the reader dec.in.
// The goal is to reduce memory allocs.
func (dec *decoder) decodeU() uint32 {
	dec.align(4)
	dec.binread(&dec.u)
	dec.pos += 4
	return dec.u
}

func (dec *decoder) decode(s string, depth int) interface{} {
	dec.align(alignment(typeFor(s)))
	switch s[0] {
	case 'y':
		if _, err := dec.in.Read(dec.y[:]); err != nil {
			panic(err)
		}
		dec.pos++
		return dec.y[0]
	case 'b':
		switch dec.decodeU() {
		case 0:
			return false
		case 1:
			return true
		default:
			panic(FormatError("invalid value for boolean"))
		}
	case 'n':
		dec.binread(&dec.n)
		dec.pos += 2
		return dec.n
	case 'i':
		dec.binread(&dec.i)
		dec.pos += 4
		return dec.i
	case 'x':
		dec.binread(&dec.x)
		dec.pos += 8
		return dec.x
	case 'q':
		dec.binread(&dec.q)
		dec.pos += 2
		return dec.q
	case 'u':
		return dec.decodeU()
	case 't':
		dec.binread(&dec.t)
		dec.pos += 8
		return dec.t
	case 'd':
		dec.binread(&dec.d)
		dec.pos += 8
		return dec.d
	case 's':
		length := dec.decodeU()
		p := int(length) + 1
		if err := dec.read2buf(p); err != nil {
			panic(err)
		}
		dec.pos += p
		return string(dec.buf[:len(dec.buf)-1])
	case 'o':
		return ObjectPath(dec.decode("s", depth).(string))
	case 'g':
		length := dec.decode("y", depth).(byte)
		p := int(length) + 1
		if err := dec.read2buf(p); err != nil {
			panic(err)
		}
		dec.pos += p
		sig, err := ParseSignature(
			string(dec.buf[:len(dec.buf)-1]),
		)
		if err != nil {
			panic(err)
		}
		return sig
	case 'v':
		if depth >= 64 {
			panic(FormatError("input exceeds container depth limit"))
		}
		var variant Variant
		sig := dec.decode("g", depth).(Signature)
		if len(sig.str) == 0 {
			panic(FormatError("variant signature is empty"))
		}
		err, rem := validSingle(sig.str, &depthCounter{})
		if err != nil {
			panic(err)
		}
		if rem != "" {
			panic(FormatError("variant signature has multiple types"))
		}
		variant.sig = sig
		variant.value = dec.decode(sig.str, depth+1)
		return variant
	case 'h':
		idx := dec.decodeU()
		if int(idx) < len(dec.fds) {
			return UnixFD(dec.fds[idx])
		}
		return UnixFDIndex(idx)
	case 'a':
		if len(s) > 1 && s[1] == '{' {
			ksig := s[2:3]
			vsig := s[3 : len(s)-1]
			v := reflect.MakeMap(reflect.MapOf(typeFor(ksig), typeFor(vsig)))
			if depth >= 63 {
				panic(FormatError("input exceeds container depth limit"))
			}
			length := dec.decodeU()
			// Even for empty maps, the correct padding must be included
			dec.align(8)
			spos := dec.pos
			for dec.pos < spos+int(length) {
				dec.align(8)
				if !isKeyType(v.Type().Key()) {
					panic(InvalidTypeError{v.Type()})
				}
				kv := dec.decode(ksig, depth+2)
				vv := dec.decode(vsig, depth+2)
				v.SetMapIndex(reflect.ValueOf(kv), reflect.ValueOf(vv))
			}
			return v.Interface()
		}
		if depth >= 64 {
			panic(FormatError("input exceeds container depth limit"))
		}
		sig := s[1:]
		length := dec.decodeU()
		// capacity can be determined only for fixed-size element types
		var capacity int
		if s := sigByteSize(sig); s != 0 {
			capacity = int(length) / s
		}
		v := reflect.MakeSlice(reflect.SliceOf(typeFor(sig)), 0, capacity)
		// Even for empty arrays, the correct padding must be included
		align := alignment(typeFor(s[1:]))
		if len(s) > 1 && s[1] == '(' {
			// Special case for arrays of structs
			// structs decode as a slice of interface{} values
			// but the dbus alignment does not match this
			align = 8
		}
		dec.align(align)
		spos := dec.pos
		for dec.pos < spos+int(length) {
			ev := dec.decode(s[1:], depth+1)
			v = reflect.Append(v, reflect.ValueOf(ev))
		}
		return v.Interface()
	case '(':
		if depth >= 64 {
			panic(FormatError("input exceeds container depth limit"))
		}
		dec.align(8)
		v := make([]interface{}, 0)
		s = s[1 : len(s)-1]
		for s != "" {
			err, rem := validSingle(s, &depthCounter{})
			if err != nil {
				panic(err)
			}
			ev := dec.decode(s[:len(s)-len(rem)], depth+1)
			v = append(v, ev)
			s = rem
		}
		return v
	default:
		panic(SignatureError{Sig: s})
	}
}

// sigByteSize tries to calculates size of the given signature in bytes.
//
// It returns zero when it can't, for example when it contains non-fixed size
// types such as strings, maps and arrays that require reading of the transmitted
// data, for that we would need to implement the unread method for Decoder first.
func sigByteSize(sig string) int {
	var total int
	for offset := 0; offset < len(sig); {
		switch sig[offset] {
		case 'y':
			total += 1
			offset += 1
		case 'n', 'q':
			total += 2
			offset += 1
		case 'b', 'i', 'u', 'h':
			total += 4
			offset += 1
		case 'x', 't', 'd':
			total += 8
			offset += 1
		case '(':
			i := 1
			depth := 1
			for i < len(sig[offset:]) && depth != 0 {
				if sig[offset+i] == '(' {
					depth++
				} else if sig[offset+i] == ')' {
					depth--
				}
				i++
			}
			s := sigByteSize(sig[offset+1 : offset+i-1])
			if s == 0 {
				return 0
			}
			total += s
			offset += i
		default:
			return 0
		}
	}
	return total
}

// A FormatError is an error in the wire format.
type FormatError string

func (e FormatError) Error() string {
	return "dbus: wire format error: " + string(e)
}
