package fasthttp2

import (
	"crypto/tls"
	"net"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

// TODO: checkout https://github.com/golang/net/blob/4acb7895a057/http2/transport.go#L570
func ConfigureClient(c *fasthttp.HostClient) error {
	tlsConfig := c.TLSConfig

	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			MaxVersion: tls.VersionTLS13,
			MinVersion: tls.VersionTLS12,
		}
	}

	emptyServerName := len(tlsConfig.ServerName) == 0
	if emptyServerName {
		host, _, err := net.SplitHostPort(c.Addr)
		if err != nil {
			host = c.Addr
		}

		tlsConfig.ServerName = host
	}

	tlsConfig.NextProtos = append(tlsConfig.NextProtos, "h2")

	c2, err := http2.Dial(c.Addr, tlsConfig)
	if err != nil {
		if err == http2.ErrServerSupport && c.TLSConfig != nil { // remove added config settings
			tlsConfig.NextProtos = tlsConfig.NextProtos[:len(tlsConfig.NextProtos)-1]
			if emptyServerName {
				tlsConfig.ServerName = ""
			}
		}

		return err
	}

	c.TLSConfig = tlsConfig

	c.Transport = Do(c2)

	return nil
}

func Do(c *http2.Client) fasthttp.TransportFunc {
	return func(req *fasthttp.Request, res *fasthttp.Response) error {
		ch := make(chan error, 1)
		c.Register(&ClientAdaptor{
			req: req,
			res: res,
			ch:  ch,
		})

		var err error
		select {
		case err = <-ch:
		}

		return err
	}
}
