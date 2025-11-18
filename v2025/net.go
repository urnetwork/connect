package connect

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"golang.org/x/net/proxy"
	// "github.com/golang/glog"
)

type DialContextFunction = func(ctx context.Context, network string, addr string) (net.Conn, error)
type DialTlsContextFunction = func(ctx context.Context, network string, addr string) (net.Conn, error)

func DefaultConnectSettings() *ConnectSettings {
	tlsConfig, err := DefaultTlsConfig()
	if err != nil {
		panic(err)
	}
	return &ConnectSettings{
		RequestTimeout:   15 * time.Second,
		ConnectTimeout:   5 * time.Second,
		TlsTimeout:       5 * time.Second,
		HandshakeTimeout: 5 * time.Second,
		IdleConnTimeout:  90 * time.Second,
		KeepAliveTimeout: 5 * time.Second,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     5 * time.Second,
			Interval: 5 * time.Second,
			Count:    1,
		},
		TlsConfig: tlsConfig,
	}
}

type ConnectSettings struct {
	RequestTimeout   time.Duration
	ConnectTimeout   time.Duration
	TlsTimeout       time.Duration
	HandshakeTimeout time.Duration
	IdleConnTimeout  time.Duration
	KeepAliveTimeout time.Duration
	KeepAliveConfig  net.KeepAliveConfig

	TlsConfig *tls.Config

	ProxySettings *ProxySettings
	Resolver      *net.Resolver
}

func (self *ConnectSettings) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	netDialer := self.NetDialer()

	var dialContext DialContextFunction
	if self.ProxySettings != nil {
		dialContext = self.ProxySettings.NewDialContext(
			ctx,
			netDialer,
		)
	} else {
		dialContext = netDialer.DialContext
	}

	return dialContext(ctx, network, addr)
}

func (self *ConnectSettings) NetDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:         self.ConnectTimeout,
		KeepAlive:       self.KeepAliveTimeout,
		KeepAliveConfig: self.KeepAliveConfig,
		Resolver:        self.Resolver,
	}
}

type ProxySettings struct {
	Network string
	Address string
	Auth    *proxy.Auth
}

func (self *ProxySettings) NewDialContext(ctx context.Context, forward proxy.Dialer) DialContextFunction {
	return func(ctx context.Context, network string, addr string) (net.Conn, error) {
		proxyDialer, err := proxy.SOCKS5(
			self.Network,
			self.Address,
			self.Auth,
			forward,
		)
		if err != nil {
			return nil, err
		}

		var conn net.Conn
		if v, ok := proxyDialer.(proxy.ContextDialer); ok {
			conn, err = v.DialContext(ctx, network, addr)
		} else {
			conn, err = proxyDialer.Dial(network, addr)
		}
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
}
