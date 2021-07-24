package http2

import (
	"crypto/tls"
	"errors"
	"net"

	"github.com/valyala/fasthttp"
)

var (
	ErrServerSupport = errors.New("server doesn't support HTTP/2")
)

func configureDialer(d *Dialer) {
	tlsConfig := d.TLSConfig

	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			MaxVersion: tls.VersionTLS13,
			MinVersion: tls.VersionTLS12,
		}
	}

	emptyServerName := len(tlsConfig.ServerName) == 0
	if emptyServerName {
		host, _, err := net.SplitHostPort(d.Addr)
		if err != nil {
			host = d.Addr
		}

		tlsConfig.ServerName = host
	}

	tlsConfig.NextProtos = append(tlsConfig.NextProtos, "h2")
}

func ConfigureClient(c *fasthttp.HostClient) error {
	tlsConfig := c.TLSConfig
	emptyServerName := len(tlsConfig.ServerName) == 0

	d := Dialer{
		Addr:      c.Addr,
		TLSConfig: tlsConfig,
	}

	_, err := d.tryDial()
	if err != nil {
		if err == ErrServerSupport && c.TLSConfig != nil { // remove added config settings
			tlsConfig.NextProtos = tlsConfig.NextProtos[:len(tlsConfig.NextProtos)-1]
			if emptyServerName {
				tlsConfig.ServerName = ""
			}
		}

		return err
	}

	c.TLSConfig = tlsConfig

	return nil
}

var ErrNotAvailableStreams = errors.New("ran out of available streams")
