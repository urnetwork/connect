package connect

import (
	"testing"
)

func TestConnectHost(t *testing.T) {

	host, err := connectHost("http://connect.foo.bar")
	AssertEqual(t, err, nil)
	AssertEqual(t, host, "connect.foo.bar")

	host, err = connectHost("https://other-connect.bar.com")
	AssertEqual(t, err, nil)
	AssertEqual(t, host, "other-connect.bar.com")
}

func TestConnectPumpHost(t *testing.T) {
	host, err := pumpHost("http://connect.foo.bar", []byte("foo.com"))
	AssertEqual(t, err, nil)
	AssertEqual(t, host, "zone-foo-com.foo.bar")

	host, err = pumpHost("http://127.0.0.1", []byte("foo.com"))
	AssertEqual(t, err, nil)
	AssertEqual(t, host, "127.0.0.1")
}
