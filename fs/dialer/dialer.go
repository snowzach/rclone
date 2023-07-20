package dialer

import (
	"context"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/net/proxy"
)

type contextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type dialer interface {
	Dial(network, address string) (net.Conn, error)
}

// Dialer structure contains our connection dialer and timeout, tclass support
type Dialer struct {
	dialer  dialer
	timeout time.Duration
	tclass  int
}

// NewDialer creates a Dialer structure with Timeout, Keepalive,
// LocalAddr and DSCP set from rclone flags as well as optional
// socks proxy support.
func NewDialer(ctx context.Context) *Dialer {
	ci := fs.GetConfig(ctx)

	baseDialer := &net.Dialer{
		Timeout:   ci.ConnectTimeout,
		KeepAlive: 30 * time.Second,
	}
	if ci.BindAddr != nil {
		baseDialer.LocalAddr = &net.TCPAddr{IP: ci.BindAddr}
	}

	if ci.Socks5Proxy != "" {
		// Parse out the address and optional credentials
		// user:pass@host
		// user@host
		// host
		var (
			proxyAddress string
			proxyAuth    *proxy.Auth
		)
		if credsAndHost := strings.SplitN(ci.Socks5Proxy, "@", 2); len(credsAndHost) == 2 {
			proxyCreds := strings.SplitN(credsAndHost[0], ":", 2)
			proxyAuth = &proxy.Auth{
				User: proxyCreds[0],
			}
			if len(proxyCreds) == 2 {
				proxyAuth.Password = proxyCreds[1]
			}
			proxyAddress = credsAndHost[1]
		} else {
			proxyAddress = credsAndHost[0]
		}
		// Build the proxy dialer
		d, _ := proxy.SOCKS5("tcp", proxyAddress, proxyAuth, baseDialer)
		return &Dialer{
			dialer:  d,
			timeout: ci.Timeout,
			tclass:  int(ci.TrafficClass),
		}
	} else {
		return &Dialer{
			dialer:  baseDialer,
			timeout: ci.Timeout,
			tclass:  int(ci.TrafficClass),
		}
	}
}

// Dial connects to the network address.
func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

var warnDSCPFail, warnDSCPWindows sync.Once

// DialContext connects to the network address using the provided context.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {

	var (
		c   net.Conn
		err error
	)
	if dc, ok := d.dialer.(contextDialer); ok {
		c, err = dc.DialContext(ctx, network, address)
	} else {
		// If the dialer does not natively support DialContext, use the wrapper function.
		c, err = dialContext(ctx, d.dialer, network, address)
	}
	if err != nil {
		return c, err
	}

	if d.tclass != 0 {
		// IPv6 addresses must have two or more ":"
		if strings.Count(c.RemoteAddr().String(), ":") > 1 {
			err = ipv6.NewConn(c).SetTrafficClass(d.tclass)
		} else {
			err = ipv4.NewConn(c).SetTOS(d.tclass)
			// Warn of silent failure on Windows (IPv4 only, IPv6 caught by error handler)
			if runtime.GOOS == "windows" {
				warnDSCPWindows.Do(func() {
					fs.LogLevelPrintf(fs.LogLevelWarning, nil, "dialer: setting DSCP on Windows/IPv4 fails silently; see https://github.com/golang/go/issues/42728")
				})
			}
		}
		if err != nil {
			warnDSCPFail.Do(func() {
				fs.LogLevelPrintf(fs.LogLevelWarning, nil, "dialer: failed to set DSCP socket options: %v", err)
			})
		}
	}

	t := &timeoutConn{
		Conn:    c,
		timeout: d.timeout,
	}
	return t, t.nudgeDeadline()
}

// WARNING: this can leak a goroutine for as long as the underlying Dialer implementation takes to timeout
// A Conn returned from a successful Dial after the context has been cancelled will be immediately closed.
func dialContext(ctx context.Context, d dialer, network, address string) (net.Conn, error) {
	var (
		conn net.Conn
		done = make(chan struct{}, 1)
		err  error
	)
	go func() {
		conn, err = d.Dial(network, address)
		close(done)
		if conn != nil && ctx.Err() != nil {
			conn.Close()
		}
	}()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-done:
	}
	return conn, err
}
