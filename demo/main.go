package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgrr/websocket"
	"github.com/domsolutions/http2"
	"github.com/fasthttp/router"
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
				Min: image.Point{X: xi * tileSize, Y: yi * tileSize},
				Max: image.Point{X: (xi + 1) * tileSize, Y: (yi + 1) * tileSize},
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
		io.WriteString(ctx, `<p><div id='loadtimes'></div></p><br><div id="rtt"></div>
<script>
function showtimes() {
	var times = 'Times from connection start:<br>'
	times += 'DOM loaded: ' + (window.performance.timing.domContentLoadedEventEnd - window.performance.timing.connectStart) + 'ms<br>'
	times += 'DOM complete (images loaded): ' + (window.performance.timing.domComplete - window.performance.timing.connectStart) + 'ms<br>'
	document.getElementById('loadtimes').innerHTML = times
}

var ws = new WebSocket("wss://http2.gofiber.io/ws");
ws.onmessage = function(e) {
    var data = JSON.parse(e.data)
	document.getElementById("rtt").innerHTML = 'RTT: ' + data.rtt_in_ms;
}

ws.onclose = function(e){
	document.getElementById("rtt").innerHTML = "CLOSED";
}
</script>
<hr><a href='/'>&lt;&lt Back to Go HTTP/2 demo server</a></body></html>`)
	}
}

type WebSocketService struct {
	connCount int64
	conns     sync.Map
	once      sync.Once
}

func (ws *WebSocketService) OnOpen(c *websocket.Conn) {
	ws.conns.Store(c.ID(), c)

	log.Printf("New connection %s. Total connections %d\n",
		c.RemoteAddr(), atomic.AddInt64(&ws.connCount, 1))

	ws.once.Do(ws.Run)
}

func (ws *WebSocketService) OnClose(c *websocket.Conn, err error) {
	if err != nil {
		log.Printf("Closing %s with error %s\n", c.RemoteAddr(), err)
	} else {
		log.Printf("Closing %s\n", c.RemoteAddr())
	}

	log.Printf("Connections left %d\n", atomic.AddInt64(&ws.connCount, -1))

	ws.conns.Delete(c.ID())
}

type rttMessage struct {
	RTTInMs int64 `json:"rtt_in_ms"`
}

func (ws *WebSocketService) OnPong(c *websocket.Conn, data []byte) {
	if len(data) != 8 {
		return
	}

	ts := time.Now().Sub(
		time.Unix(0, int64(
			binary.BigEndian.Uint64(data))),
	)

	data, _ = json.Marshal(
		rttMessage{
			RTTInMs: ts.Milliseconds(),
		})

	c.Write(data)
}

func (ws *WebSocketService) Run() {
	time.AfterFunc(time.Millisecond*500, func() {
		var tsData [8]byte

		binary.BigEndian.PutUint64(
			tsData[:], uint64(time.Now().UnixNano()))

		ws.conns.Range(func(_, v interface{}) bool {
			c := v.(*websocket.Conn)

			c.Ping(tsData[:])

			log.Printf("Sending ping to %d: %s\n", c.ID(), c.RemoteAddr())

			return true
		})

		ws.Run()
	})
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
	service := &WebSocketService{}

	ws := websocket.Server{}

	ws.HandleOpen(service.OnOpen)
	ws.HandlePong(service.OnPong)
	ws.HandleClose(service.OnClose)

	r := router.New()
	r.GET("/ws", ws.Upgrade)

	r.NotFound = newBTCTiles()

	s := &fasthttp.Server{
		Handler: r.Handler,
		Name:    "HTTP2 Demo",
	}

	http2.ConfigureServer(s)

	err := s.ListenAndServeTLS(*listenArg, *certArg, *keyArg)
	if err != nil {
		log.Fatalln(err)
	}
}
