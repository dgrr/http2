package fasthttp2

import (
	"crypto/tls"

	"github.com/valyala/fasthttp"
)

// TODO: checkout https://github.com/golang/net/blob/4acb7895a057/http2/transport.go#L570
func ConfigureClient(c *fasthttp.HostClient) error {
	if c.TLSConfig == nil {
		c.TLSConfig = &tls.Config{
			MaxVersion: tls.VersionTLS13,
			MinVersion: tls.VersionTLS12,
		}
	}

	c.TLSConfig.NextProtos = append(c.TLSConfig.NextProtos, "h2")

	c2, err := Dial(c.Addr, c.TLSConfig)
	if err != nil {
		return err
	}

	c.Transport = c2.Do

	return nil
}
