package connect

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/glog"
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
		ConnectTimeout:   15 * time.Second,
		TlsTimeout:       15 * time.Second,
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

	DialContextSettings *DialContextSettings

	DisableIpv4 bool
	DisableIpv6 bool
}

type DialContextSettings struct {
	DialContext DialContextFunction
}

func (self *ConnectSettings) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	if self.DisableIpv4 && self.DisableIpv6 {
		return nil, fmt.Errorf("ipv4 and ipv6 are both disabled")
	}
	switch network {
	case "tcp":
		if self.DisableIpv4 {
			network = "tcp6"
		} else if self.DisableIpv6 {
			network = "tcp4"
		}
	case "tcp4":
		if self.DisableIpv4 {
			return nil, fmt.Errorf("ipv4 is disabled")
		}
	case "tcp6":
		if self.DisableIpv6 {
			return nil, fmt.Errorf("ipv6 is disabled")
		}
	case "udp":
		if self.DisableIpv4 {
			network = "udp6"
		} else if self.DisableIpv6 {
			network = "udp4"
		}
	case "udp4":
		if self.DisableIpv4 {
			return nil, fmt.Errorf("ipv4 is disabled")
		}
	case "udp6":
		if self.DisableIpv6 {
			return nil, fmt.Errorf("ipv6 is disabled")
		}
	}

	var dialContext DialContextFunction

	if self.DialContextSettings != nil {
		dialContext = self.DialContextSettings.DialContext
	} else {
		netDialer := self.NetDialer()
		if self.ProxySettings != nil {
			dialContext = self.ProxySettings.NewDialContext(
				ctx,
				netDialer,
			)
		} else {
			dialContext = netDialer.DialContext
		}
	}

	conn, err := dialContext(ctx, network, addr)
	if glog.V(2) {
		if err == nil {
			glog.Infof("[net]dial %s %s success\n", network, addr)
		} else {
			glog.Infof("[net]dial %s %s err=%s\n", network, addr, err)
		}
	}
	return conn, err
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
