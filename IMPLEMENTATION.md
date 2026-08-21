# Implementation

A document that explains in detail how the client and the server works.

## Client implementation

The client holds (0, N) connections to a single host.
A connection is created in the following cases:
- There are no previous existing connections.
- All the connections are busy (aka not able to open more streams).

Connections are stored in a list because it's the easiest way to keep elements.

When a connection is created 2 goroutines are spawned. One for reading
and dispatching events, and another for writing (either frames and requests).

The [read loop](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L357)
will read all the frames and handling only the ones carrying a StreamID.
Lower layers will handle everything related to Settings, WindowUpdate, Ping
and/or disconnection.

The [write loop](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L290)
will write the requests and frames. I like to separate both terms because the request
comes from fasthttp, and the `frames` is a term related to http2.

Why having 2 coroutines? As HTTP/2 is a replacement of HTTP/1.1, the equivalent
to opening a connection per request in HTTP/1 is the figure of the `frame` in HTTP/2.
As writing to the same connection might happen concurrently and thus, can invoke
errors, 2 coroutines are required, one for writing and another for reading
synchronously.

### How sending a request works?

When we send a request we write to a channel to the writeLoop coroutine with
all the data required, in this case we make use of the [Ctx](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/client.go#L26-L33)
structure.

That being sent, it gets received by the writeLoop coroutine, and then
it proceeds to [serialize and write](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L385)
into the connection the required frames, and after that [registers](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L321)
the StreamID into a shared map. This map is shared among the 'write' and 'read' loops.

In the meantime, the client [waits on a channel](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/client.go#L102)
for any error.

When we receive the response from the server, the readLoop will check if the StreamID
is on the shared map, and if so, it will [handle the response](https://github.com/dgrr/http2/blob/8cb32376c36f056fca0ec30854f3522005a777ac/conn.go#L559).
After the server finished sending the request, the readLoop will end the request
sending the result to the client. That result might be an error or just a `nil`
over the channel provided by the client.

After the request/response finished, the client will continue thus exiting the
`Do` function.

## Flow control

Both ends meter what they send against the peer's windows, which is what the
protocol requires and what any peer that enforces the rule expects.

On the client, a request body larger than the window is not written in one go.
The part that does not fit stays with the request, and the write loop sends more
of it when a WINDOW_UPDATE opens the window. A change to
SETTINGS_INITIAL_WINDOW_SIZE is applied as a delta to every stream that is
already open, as RFC 7540 6.9.2 requires.

On the server the same applies to responses, both the ones the handler buffers
and the ones it streams. A streamed body is pulled from the handler's reader a
frame at a time as the windows allow rather than drained into the writer, so a
peer with a small window is not flooded and a large response does not have to be
held in memory.

In the other direction the server hands its receive window straight back as the
bytes are buffered, so it never becomes the thing that limits an upload. What
bounds the memory instead is MaxRequestBodySize per stream and
SETTINGS_MAX_CONCURRENT_STREAMS across the connection.

## Server implementation

A connection runs three goroutines: one reading frames off the socket, one
writing them back, and one, the stream loop, that owns everything else. The
stream table, the HPACK encoder and decoder, the flow-control windows and the
closed-stream ring are all only ever touched by that loop, which is what keeps
them free of locks.

Handlers do not run there. When a request is complete the loop hands it to a
goroutine of its own and carries straight on serving the other streams, and the
handler reports back on a channel when it returns. The response is turned into
frames back on the loop, so the HPACK encoder still has a single writer and the
header blocks still reach the wire in the order they were encoded.

Running the handler inline, which is what this used to do, meant a slow request
held up every other stream on the connection. Multiplexing exists to avoid
exactly that, so a connection was no better than an HTTP/1 one and, since the
peer sends everything down a single socket, often worse.

### What bounds the concurrency

A handler goroutine holds its stream's slot until it returns, so
SETTINGS_MAX_CONCURRENT_STREAMS is the ceiling on handlers in flight per
connection, not just on streams the peer may have open.

The distinction matters for CVE-2023-44487. An attacker sends a complete
request and cancels it immediately: the handler is already running, and if
cancelling gave the slot straight back, every RST_STREAM would buy another
goroutine while the peer never appeared to exceed the limit. Holding the slot
until the handler finishes is what makes the advertised limit the real one, and
it needs no rate limiting or heuristics to do it.

A stream cancelled while its handler runs is marked abandoned rather than
recycled: the handler still owns the RequestCtx, and returning it to the pool
would hand the same one to another stream mid-request. Whatever the handler
produces is dropped when it reports back.
