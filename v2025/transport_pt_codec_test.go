package connect

import (
	mathrand "math/rand"
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestDnsRequestEncodeDecode(t *testing.T) {
	var encodeBuf [1024]byte
	var decodeBuf [1024]byte

	tld := []byte("a.dev.")
	tlds := [][]byte{
		[]byte("b.dev."),
		[]byte("c.bar."),
		[]byte("a.dev."),
	}

	for range 32 {
		rlen := 4*1024 + mathrand.Intn(32*1024)
		data := make([]byte, rlen)
		mathrand.Read(data)

		c := 0
		i := 0
		for i < len(data) {

			var header [18]byte
			mathrand.Read(header[0:16])
			header[16] = uint8(c)
			header[17] = 0

			n, encoded, err := encodeDnsRequest(uint16(i), header, data[i:], encodeBuf, tld)
			assert.Equal(t, err, nil)

			id, decodedHeader, decoded, decodedTld, err, otherData := decodeDnsRequest(encoded, decodeBuf, tlds)
			assert.Equal(t, err, nil)
			assert.Equal(t, id, uint16(i))
			assert.Equal(t, data[i:i+n], decoded)
			assert.Equal(t, header, decodedHeader)
			assert.Equal(t, decodedTld, tld)
			assert.Equal(t, otherData, false)

			i += n
			c += 1
		}

		assert.Equal(t, encodeDnsRequestCount(data, tld), c)
	}

	var header [18]byte
	mathrand.Read(header[0:16])
	header[16] = 0
	header[17] = 0

	_, encoded, err := encodeDnsRequest(uint16(0), header, make([]byte, 0), encodeBuf, tld)
	assert.Equal(t, err, nil)

	id, decodedHeader, decoded, decodedTld, err, otherData := decodeDnsRequest(encoded, decodeBuf, tlds)
	assert.Equal(t, err, nil)
	assert.Equal(t, id, uint16(0))
	assert.Equal(t, make([]byte, 0), decoded)
	assert.Equal(t, header, decodedHeader)
	assert.Equal(t, decodedTld, tld)
	assert.Equal(t, otherData, false)
}

func TestDnsResponseEncodeDecode(t *testing.T) {
	var encodeBuf [1024]byte
	var decodeBuf [1024]byte

	tld := []byte("a.dev.")
	tlds := [][]byte{
		[]byte("b.dev."),
		[]byte("c.bar."),
		[]byte("a.dev."),
	}

	for range 32 {
		rlen := 4*1024 + mathrand.Intn(32*1024)
		data := make([]byte, rlen)
		mathrand.Read(data)

		c := 0
		i := 0
		for i < len(data) {
			var header [18]byte
			mathrand.Read(header[0:16])
			header[16] = uint8(c)
			header[17] = 0

			n, encoded, err := encodeDnsResponse(uint16(i), header, header, data[i:], encodeBuf, tld)
			assert.Equal(t, err, nil)

			id, decodedPumpHeader, decodedHeader, decoded, err := decodeDnsResponse(encoded, decodeBuf, tlds)
			assert.Equal(t, err, nil)
			assert.Equal(t, id, uint16(i))
			assert.Equal(t, data[i:i+n], decoded)
			assert.Equal(t, header, decodedPumpHeader)
			assert.Equal(t, header, decodedHeader)

			i += n
			c += 1
		}

		assert.Equal(t, encodeDnsResponseCount(data, tld), c)
	}

	var header [18]byte
	mathrand.Read(header[0:16])
	header[16] = 0
	header[17] = 0

	_, encoded, err := encodeDnsResponse(uint16(0), header, header, make([]byte, 0), encodeBuf, tld)
	assert.Equal(t, err, nil)

	id, decodedPumpHeader, decodedHeader, decoded, err := decodeDnsResponse(encoded, decodeBuf, tlds)
	assert.Equal(t, err, nil)
	assert.Equal(t, id, uint16(0))
	assert.Equal(t, make([]byte, 0), decoded)
	assert.Equal(t, header, decodedPumpHeader)
	assert.Equal(t, header, decodedHeader)
}
