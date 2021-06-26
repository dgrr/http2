package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"strconv"
	"time"

	"github.com/dgrr/http2/fasthttp2"
	"github.com/valyala/fasthttp"
)

func newBTCTiles() fasthttp.RequestHandler {
	const btcURL = "https://www.phneep.com/wp-content/uploads/2019/09/1-Strong-Hands-Bitcoin-web.jpg"
	statusCode, slurp, err := fasthttp.Get(nil, btcURL)
	if err != nil {
		log.Fatal(err)
	}
	if statusCode != 200 {
		log.Fatalf("Error fetching %s", btcURL)
	}

	im, err := jpeg.Decode(bytes.NewReader(slurp))
	if err != nil {
		if len(slurp) > 1024 {
			slurp = slurp[:1024]
		}
		log.Fatalf("Failed to decode gopher image: %v (got %q)", err, slurp)
	}

	type subImager interface {
		SubImage(image.Rectangle) image.Image
	}

	const tileSize = 32
	xt := im.Bounds().Max.X / tileSize
	yt := im.Bounds().Max.Y / tileSize
	var tile [][][]byte // y -> x -> jpeg bytes
	for yi := 0; yi < yt; yi++ {
		var row [][]byte
		for xi := 0; xi < xt; xi++ {
			si := im.(subImager).SubImage(image.Rectangle{
				Min: image.Point{xi * tileSize, yi * tileSize},
				Max: image.Point{(xi + 1) * tileSize, (yi + 1) * tileSize},
			})
			buf := new(bytes.Buffer)
			if err := jpeg.Encode(buf, si, &jpeg.Options{Quality: 90}); err != nil {
				log.Fatal(err)
			}
			row = append(row, buf.Bytes())
		}
		tile = append(tile, row)
	}

	return func(ctx *fasthttp.RequestCtx) {
		ms, _ := strconv.Atoi(string(ctx.FormValue("latency")))
		const nanosPerMilli = 1e6
		if ctx.FormValue("x") != nil {
			x, _ := strconv.Atoi(string(ctx.FormValue("x")))
			y, _ := strconv.Atoi(string(ctx.FormValue("y")))
			if ms <= 1000 {
				time.Sleep(time.Duration(ms) * nanosPerMilli)
			}
			if x >= 0 && x < xt && y >= 0 && y < yt {
				ctx.SetContentType("image/jpeg")
				ctx.Write(tile[y][x])
			}
			return
		}
		ctx.SetContentType("text/html; charset=utf-8")
		io.WriteString(ctx, "<html><body onload='showtimes()'>")
		fmt.Fprintf(ctx, "A grid of %d tiled images is below. Compare:<p>", xt*yt)
		for _, ms := range []int{0, 30, 200, 1000} {
			d := time.Duration(ms) * nanosPerMilli
			fmt.Fprintf(ctx, "[<a href='https://%s/tiles?latency=%d'>HTTP/2, %v latency</a>] [<a href='http://%s/tiles?latency=%d'>HTTP/1, %v latency</a>]<br>\n",
				"http2.gofiber.io", ms, d,
				"not_found.hehe", ms, d,
			)
		}
		io.WriteString(ctx, "<p>\n")
		cacheBust := time.Now().UnixNano()
		for y := 0; y < yt; y++ {
			for x := 0; x < xt; x++ {
				fmt.Fprintf(ctx, "<img width=%d height=%d src='/tiles?x=%d&y=%d&cachebust=%d&latency=%d'>",
					tileSize, tileSize, x, y, cacheBust, ms)
			}
			io.WriteString(ctx, "<br/>\n")
		}
		io.WriteString(ctx, `<p><div id='loadtimes'></div></p>
<script>
function showtimes() {
	var times = 'Times from connection start:<br>'
	times += 'DOM loaded: ' + (window.performance.timing.domContentLoadedEventEnd - window.performance.timing.connectStart) + 'ms<br>'
	times += 'DOM complete (images loaded): ' + (window.performance.timing.domComplete - window.performance.timing.connectStart) + 'ms<br>'
	document.getElementById('loadtimes').innerHTML = times
}
</script>
<hr><a href='/'>&lt;&lt Back to Go HTTP/2 demo server</a></body></html>`)
	}
}

var (
	certArg   = flag.String("cert", "", "idk")
	keyArg    = flag.String("key", "", "idk")
	listenArg = flag.String("addr", ":8443", "idk")
)

func init() {
	flag.Parse()
}

func main() {
	s := &fasthttp.Server{
		Handler: newBTCTiles(),
		Name:    "HTTP2 Demo",
	}

	fasthttp2.ConfigureServer(s)

	err := s.ListenAndServeTLS(*listenArg, *certArg, *keyArg)
	if err != nil {
		log.Fatalln(err)
	}
}
