package main

import (
	mathrand "math/rand"
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestEncodeDecode(t *testing.T) {

	var encodeBuf [512]byte
	var decodeBuf [512]byte

	tld := []byte("ww.dev.")
	tlds := [][]byte{
		[]byte("vpn.dev."),
		[]byte("foo.bar."),
		[]byte("ww.dev."),
	}

	for range 32 {
		rlen := 4*1024 + mathrand.Intn(32*1024)
		data := make([]byte, rlen)
		mathrand.Read(data)

		c := 0
		i := 0
		for i < len(data) {

			var header [18]byte
			header[0] = 0
			header[1] = uint8(c)
			mathrand.Read(header[2:18])

			n, encoded, err := encode(uint16(i), header, data[i:], encodeBuf, tld)
			assert.Equal(t, err, nil)

			id, decodedHeader, decoded, err := decode(encoded, decodeBuf, tlds)
			assert.Equal(t, err, nil)
			assert.Equal(t, id, uint16(i))
			assert.Equal(t, data[i:i+n], decoded)
			assert.Equal(t, header, decodedHeader)

			i += n
			c += 1

		}

		assert.Equal(t, encodeCount(data, tld), c)
	}

}
