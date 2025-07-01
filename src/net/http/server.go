// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// HTTP server. See RFC 7230 through 7235.

package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"internal/godebug"
	"io"
	"log"
	"maps"
	"math/rand"
	"net"
	"net/textproto"
	"net/url"
	urlpkg "net/url"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe" // for linkname

	"golang.org/x/net/http/httpguts"
)

// Errors used by the HTTP server.
var (
	// ErrBodyNotAllowed is returned by ResponseWriter.Write calls
	// when the HTTP method or response code does not permit a
	// body.
	// ErrBodyNotAllowed 是当 HTTP 方法或响应代码不允许 body 时，ResponseWriter.Write 调用返回的错误。
	ErrBodyNotAllowed = errors.New("http: request method or response status code does not allow body")

	// ErrHijacked is returned by ResponseWriter.Write calls when
	// the underlying connection has been hijacked using the
	// Hijacker interface. A zero-byte write on a hijacked
	// connection will return ErrHijacked without any other side
	// effects.
	// ErrHijacked 是当底层连接已被使用 Hijacker 接口劫持时，ResponseWriter.Write 调用返回的错误。
	// 在被劫持的连接上进行零字节写入将返回 ErrHijacked，而不会产生任何其他副作用。
	ErrHijacked = errors.New("http: connection has been hijacked")

	// ErrContentLength is returned by ResponseWriter.Write calls
	// when a Handler set a Content-Length response header with a
	// declared size and then attempted to write more bytes than
	// declared.
	// ErrContentLength 是当 Handler 设置了带有声明大小的 Content-Length 响应头，然后尝试写入比声明的字节数更多的字节时，ResponseWriter.Write 调用返回的错误。
	ErrContentLength = errors.New("http: wrote more than the declared Content-Length")

	// Deprecated: ErrWriteAfterFlush is no longer returned by
	// anything in the net/http package. Callers should not
	// compare errors against this variable.
	// Deprecated: ErrWriteAfterFlush 不再由 net/http 包中的任何内容返回。调用者不应将错误与此变量进行比较。
	ErrWriteAfterFlush = errors.New("unused")
)

// A Handler responds to an HTTP request.
//
// Handler 响应一个 HTTP 请求。
//
// [Handler.ServeHTTP] should write reply headers and data to the [ResponseWriter]
// and then return. Returning signals that the request is finished; it
// is not valid to use the [ResponseWriter] or read from the
// [Request.Body] after or concurrently with the completion of the
// ServeHTTP call.
//
// [Handler.ServeHTTP] 应该将回复头和数据写入 [ResponseWriter]，然后返回。
// 返回表示请求已完成；在 ServeHTTP 调用完成之后或与其并发地使用 [ResponseWriter] 或从 [Request.Body] 读取数据是无效的。
//
// Depending on the HTTP client software, HTTP protocol version, and
// any intermediaries between the client and the Go server, it may not
// be possible to read from the [Request.Body] after writing to the
// [ResponseWriter]. Cautious handlers should read the [Request.Body]
// first, and then reply.
//
// 根据 HTTP 客户端软件、HTTP 协议版本以及客户端和 Go 服务器之间的任何中介，
// 在写入 [ResponseWriter] 之后，可能无法从 [Request.Body] 读取数据。
// 谨慎的 Handler 应该首先读取 [Request.Body]，然后再回复。
//
// Except for reading the body, handlers should not modify the
// provided Request.
//
// 除了读取 body 之外，handler 不应修改提供的 Request。
//
// If ServeHTTP panics, the server (the caller of ServeHTTP) assumes
// that the effect of the panic was isolated to the active request.
// It recovers the panic, logs a stack trace to the server error log,
// and either closes the network connection or sends an HTTP/2
// RST_STREAM, depending on the HTTP protocol. To abort a handler so
// the client sees an interrupted response but the server doesn't log
// an error, panic with the value [ErrAbortHandler].
//
// 如果 ServeHTTP 发生 panic，服务器（ServeHTTP 的调用者）会假定 panic 的影响仅限于当前请求。
// 它会 recover 这个 panic，并将堆栈跟踪记录到服务器错误日志中，
// 并根据 HTTP 协议关闭网络连接或发送 HTTP/2 RST_STREAM。
// 要中止一个 handler，以便客户端看到一个中断的响应，但服务器不记录错误，
// 可以使用 [ErrAbortHandler] 值进行 panic。
// (译注: 正常情况下 panic 会被服务器捕获并记录错误日志, 但如果 panic 的值是 ErrAbortHandler, 则服务器不会记录错误日志)
// (译注: RST_STREAM 是 HTTP/2 协议中用于重置单个流的机制, 类似于 TCP 的 RST 报文)
// (译注: 使用 ErrAbortHandler 可以实现更细粒度的错误处理, 避免不必要的错误日志)
// (译注: 这里的 "服务器" 指的是 net/http 包中的 Server 类型, 它负责监听端口, 接受连接, 并调用 Handler 处理请求)
// (译注: "当前请求" 指的是正在被 Handler 处理的 HTTP 请求)
// (译注: "堆栈跟踪" 指的是程序在 panic 时的函数调用链, 可以帮助开发者定位问题)
// (译注: "错误日志" 指的是服务器记录错误信息的日志文件, 可以帮助开发者监控服务器的运行状态)
type Handler interface {
	ServeHTTP(ResponseWriter, *Request)
}

// A ResponseWriter interface is used by an HTTP handler to
// construct an HTTP response.
//
// A ResponseWriter may not be used after [Handler.ServeHTTP] has returned.
//
// ResponseWriter 接口被 HTTP handler 用于构建 HTTP 响应。
//
// 在 [Handler.ServeHTTP] 返回后，不得再使用 ResponseWriter。
type ResponseWriter interface {
	// Header returns the header map that will be sent by
	// [ResponseWriter.WriteHeader]. The [Header] map also is the mechanism with which
	// [Handler] implementations can set HTTP trailers.
	//
	// Changing the header map after a call to [ResponseWriter.WriteHeader] (or
	// [ResponseWriter.Write]) has no effect unless the HTTP status code was of the
	// 1xx class or the modified headers are trailers.
	//
	// There are two ways to set Trailers. The preferred way is to
	// predeclare in the headers which trailers you will later
	// send by setting the "Trailer" header to the names of the
	// trailer keys which will come later. In this case, those
	// keys of the Header map are treated as if they were
	// trailers. See the example. The second way, for trailer
	// keys not known to the [Handler] until after the first [ResponseWriter.Write],
	// is to prefix the [Header] map keys with the [TrailerPrefix]
	// constant value.
	//
	// To suppress automatic response headers (such as "Date"), set
	// their value to nil.
	//
	// Header 返回将由 [ResponseWriter.WriteHeader] 发送的 header map。
	// [Header] map 也是 [Handler] 实现可以设置 HTTP trailers 的机制。
	//
	// 在调用 [ResponseWriter.WriteHeader] (或 [ResponseWriter.Write]) 之后更改 header map 无效，
	// 除非 HTTP 状态码是 1xx 类或修改后的 headers 是 trailers。
	//
	// 有两种方法可以设置 Trailers。首选方法是在 headers 中预先声明你稍后将发送的 trailers，
	// 通过将 "Trailer" header 设置为稍后将出现的 trailer keys 的名称。
	// 在这种情况下，Header map 的这些 keys 被视为 trailers。请参见示例。
	// 第二种方法是，对于在第一次 [ResponseWriter.Write] 之后 [Handler] 才知道的 trailer keys，
	// 使用 [TrailerPrefix] 常量值作为 [Header] map keys 的前缀。
	//
	// 要禁止自动响应 headers（例如 "Date"），请将其值设置为 nil。
	Header() Header

	// Write writes the data to the connection as part of an HTTP reply.
	//
	// If [ResponseWriter.WriteHeader] has not yet been called, Write calls
	// WriteHeader(http.StatusOK) before writing the data. If the Header
	// does not contain a Content-Type line, Write adds a Content-Type set
	// to the result of passing the initial 512 bytes of written data to
	// [DetectContentType]. Additionally, if the total size of all written
	// data is under a few KB and there are no Flush calls, the
	// Content-Length header is added automatically.
	//
	// Depending on the HTTP protocol version and the client, calling
	// Write or WriteHeader may prevent future reads on the
	// Request.Body. For HTTP/1.x requests, handlers should read any
	// needed request body data before writing the response. Once the
	// headers have been flushed (due to either an explicit Flusher.Flush
	// call or writing enough data to trigger a flush), the request body
	// may be unavailable. For HTTP/2 requests, the Go HTTP server permits
	// handlers to continue to read the request body while concurrently
	// writing the response. However, such behavior may not be supported
	// by all HTTP/2 clients. Handlers should read before writing if
	// possible to maximize compatibility.
	// Write 方法将数据作为 HTTP 响应的一部分写入连接。
	//
	// 如果 [ResponseWriter.WriteHeader] 尚未被调用，Write 会在写入数据之前调用 WriteHeader(http.StatusOK)。
	// 如果 Header 中不包含 Content-Type 行，Write 会添加一个 Content-Type，其值是通过将写入数据的
	// 前 512 字节传递给 [DetectContentType] 的结果来设置的。
	// 此外，如果所有写入数据的总大小小于几 KB 并且没有 Flush 调用，则会自动添加 Content-Length header。
	//
	// 根据 HTTP 协议版本和客户端的不同，调用 Write 或 WriteHeader 可能会阻止将来对 Request.Body 的读取。
	// 对于 HTTP/1.x 请求，handlers 应该在写入响应之前读取任何需要的请求 body 数据。
	// 一旦 headers 被刷新（由于显式的 Flusher.Flush 调用或写入足够的数据以触发刷新），请求 body 可能不可用。
	// 对于 HTTP/2 请求，Go HTTP 服务器允许 handlers 在并发写入响应的同时继续读取请求 body。
	// 但是，并非所有 HTTP/2 客户端都支持这种行为。如果可能，handlers 应该在写入之前读取，以最大限度地提高兼容性。
	Write([]byte) (int, error)

	// WriteHeader sends an HTTP response header with the provided
	// status code.
	//
	// If WriteHeader is not called explicitly, the first call to Write
	// will trigger an implicit WriteHeader(http.StatusOK).
	// Thus explicit calls to WriteHeader are mainly used to
	// send error codes or 1xx informational responses.
	//
	// The provided code must be a valid HTTP 1xx-5xx status code.
	// Any number of 1xx headers may be written, followed by at most
	// one 2xx-5xx header. 1xx headers are sent immediately, but 2xx-5xx
	// headers may be buffered. Use the Flusher interface to send
	// buffered data. The header map is cleared when 2xx-5xx headers are
	// sent, but not with 1xx headers.
	//
	// The server will automatically send a 100 (Continue) header
	// on the first read from the request body if the request has
	// an "Expect: 100-continue" header.
	// WriteHeader 发送具有提供的状态码的 HTTP 响应 header。
	//
	// 如果没有显式调用 WriteHeader，则第一次调用 Write 将触发隐式的 WriteHeader(http.StatusOK)。
	// 因此，显式调用 WriteHeader 主要用于发送错误代码或 1xx 信息性响应。
	//
	// 提供的代码必须是有效的 HTTP 1xx-5xx 状态码。
	// 可以写入任意数量的 1xx header，后跟最多一个 2xx-5xx header。1xx header 立即发送，但 2xx-5xx
	// header 可能会被缓冲。使用 Flusher 接口发送缓冲的数据。当发送 2xx-5xx header 时，header map 会被清除，
	// 但 1xx header 不会。
	//
	// 如果请求具有 "Expect: 100-continue" header，服务器将在第一次从请求 body 读取时自动发送 100 (Continue) header。
	WriteHeader(statusCode int)
}

// The Flusher interface is implemented by ResponseWriters that allow
// an HTTP handler to flush buffered data to the client.
//
// The default HTTP/1.x and HTTP/2 [ResponseWriter] implementations
// support [Flusher], but ResponseWriter wrappers may not. Handlers
// should always test for this ability at runtime.
//
// Note that even for ResponseWriters that support Flush,
// if the client is connected through an HTTP proxy,
// the buffered data may not reach the client until the response
// completes.
// Flusher 接口由 ResponseWriters 实现，允许 HTTP handler 将缓冲的数据刷新到客户端。
// 默认的 HTTP/1.x 和 HTTP/2 [ResponseWriter] 实现支持 [Flusher]，但 ResponseWriter 包装器可能不支持。
// Handlers 应该始终在运行时测试此功能。
// 请注意，即使对于支持 Flush 的 ResponseWriters，如果客户端通过 HTTP 代理连接，
// 缓冲的数据也可能要到响应完成后才能到达客户端。
type Flusher interface {
	// Flush sends any buffered data to the client.
	// Flush 将任何缓冲的数据发送到客户端。
	Flush()
}

// The Hijacker interface is implemented by ResponseWriters that allow
// an HTTP handler to take over the connection.
//
// The default [ResponseWriter] for HTTP/1.x connections supports
// Hijacker, but HTTP/2 connections intentionally do not.
// ResponseWriter wrappers may also not support Hijacker. Handlers
// should always test for this ability at runtime.
type Hijacker interface {
	// Hijack lets the caller take over the connection.
	// After a call to Hijack the HTTP server library
	// will not do anything else with the connection.
	//
	// It becomes the caller's responsibility to manage
	// and close the connection.
	//
	// The returned net.Conn may have read or write deadlines
	// already set, depending on the configuration of the
	// Server. It is the caller's responsibility to set
	// or clear those deadlines as needed.
	//
	// The returned bufio.Reader may contain unprocessed buffered
	// data from the client.
	//
	// After a call to Hijack, the original Request.Body must not
	// be used. The original Request's Context remains valid and
	// is not canceled until the Request's ServeHTTP method
	// returns.
	// Hijack 允许调用者接管连接。
	// 调用 Hijack 后，HTTP 服务器库将不再对连接执行任何操作。
	// 管理和关闭连接成为调用者的责任。
	// 返回的 net.Conn 可能已经设置了读取或写入截止时间，具体取决于服务器的配置。
	// 调用者有责任根据需要设置或清除这些截止时间。
	// 返回的 bufio.Reader 可能包含来自客户端的未处理的缓冲数据。
	// 调用 Hijack 后，不得使用原始的 Request.Body。原始 Request 的 Context 仍然有效，
	// 并且在 Request 的 ServeHTTP 方法返回之前不会被取消。
	Hijack() (net.Conn, *bufio.ReadWriter, error)
}

// The CloseNotifier interface is implemented by ResponseWriters which
// allow detecting when the underlying connection has gone away.
//
// This mechanism can be used to cancel long operations on the server
// if the client has disconnected before the response is ready.
//
// Deprecated: the CloseNotifier interface predates Go's context package.
// New code should use [Request.Context] instead.
// CloseNotifier 接口由 ResponseWriters 实现，允许检测底层连接何时断开。
// 如果客户端在响应准备好之前断开连接，则可以使用此机制来取消服务器上的长时间运行的操作。
// Deprecated: CloseNotifier 接口早于 Go 的 context 包。新代码应使用 [Request.Context] 代替。
type CloseNotifier interface {
	// CloseNotify returns a channel that receives at most a
	// single value (true) when the client connection has gone
	// away.
	//
	// CloseNotify may wait to notify until Request.Body has been
	// fully read.
	//
	// After the Handler has returned, there is no guarantee
	// that the channel receives a value.
	//
	// If the protocol is HTTP/1.1 and CloseNotify is called while
	// processing an idempotent request (such as GET) while
	// HTTP/1.1 pipelining is in use, the arrival of a subsequent
	// pipelined request may cause a value to be sent on the
	// returned channel. In practice HTTP/1.1 pipelining is not
	// enabled in browsers and not seen often in the wild. If this
	// is a problem, use HTTP/2 or only use CloseNotify on methods
	// such as POST.
	// CloseNotify 返回一个通道，当客户端连接断开时，该通道最多接收一个值 (true)。
	// CloseNotify 可能会等待通知，直到 Request.Body 被完全读取。
	// 在 Handler 返回后，不能保证该通道会收到一个值。
	// 如果协议是 HTTP/1.1 并且在处理幂等请求（例如 GET）时调用 CloseNotify，同时正在使用 HTTP/1.1 管道，
	// 则后续管道请求的到达可能会导致在返回的通道上发送一个值。
	// 实际上，HTTP/1.1 管道在浏览器中未启用，并且在实际环境中很少见。
	// 如果这是一个问题，请使用 HTTP/2 或仅在 POST 等方法上使用 CloseNotify。
	CloseNotify() <-chan bool
}

var (
	// ServerContextKey is a context key. It can be used in HTTP
	// handlers with Context.Value to access the server that
	// started the handler. The associated value will be of
	// type *Server.
	// ServerContextKey 是一个 context key。它可以在 HTTP handlers 中通过 Context.Value 访问启动 handler 的 server。
	// 关联的值类型为 *Server。
	ServerContextKey = &contextKey{"http-server"}

	// LocalAddrContextKey is a context key. It can be used in
	// HTTP handlers with Context.Value to access the local
	// address the connection arrived on.
	// The associated value will be of type net.Addr.
	// LocalAddrContextKey 是一个 context key。它可以在 HTTP handlers 中通过 Context.Value 访问连接到达的本地地址。
	// 关联的值类型为 net.Addr。
	LocalAddrContextKey = &contextKey{"local-addr"}
)

// A conn represents the server side of an HTTP connection.
// conn 表示 HTTP 连接的服务器端。
type conn struct {
	// server is the server on which the connection arrived.
	// Immutable; never nil.
	// server 是连接到达的服务器。
	// 不可变；永远不为 nil。
	server *Server

	// cancelCtx cancels the connection-level context.
	// cancelCtx 取消连接级别的 context。
	cancelCtx context.CancelFunc

	// rwc is the underlying network connection.
	// This is never wrapped by other types and is the value given out
	// to CloseNotifier callers. It is usually of type *net.TCPConn or
	// *tls.Conn.
	// rwc 是底层网络连接。
	// 这永远不会被其他类型包装，并且是提供给 CloseNotifier 调用者的值。它通常是 *net.TCPConn 或 *tls.Conn 类型。
	rwc net.Conn

	// remoteAddr is rwc.RemoteAddr().String(). It is not populated synchronously
	// inside the Listener's Accept goroutine, as some implementations block.
	// It is populated immediately inside the (*conn).serve goroutine.
	// This is the value of a Handler's (*Request).RemoteAddr.
	// remoteAddr 是 rwc.RemoteAddr().String()。它不会在 Listener 的 Accept goroutine 中同步填充，因为某些实现会阻塞。
	// 它会在 (*conn).serve goroutine 中立即填充。
	// 这是 Handler 的 (*Request).RemoteAddr 的值。
	remoteAddr string

	// tlsState is the TLS connection state when using TLS.
	// nil means not TLS.
	// tlsState 是使用 TLS 时的 TLS 连接状态。
	// nil 表示未使用 TLS。
	tlsState *tls.ConnectionState

	// werr is set to the first write error to rwc.
	// It is set via checkConnErrorWriter{w}, where bufw writes.
	// werr 被设置为 rwc 的第一个写入错误。
	// 它通过 checkConnErrorWriter{w} 设置，bufw 在其中进行写入。
	werr error

	// r is bufr's read source. It's a wrapper around rwc that provides
	// io.LimitedReader-style limiting (while reading request headers)
	// and functionality to support CloseNotifier. See *connReader docs.
	// r 是 bufr 的读取源。它是 rwc 的一个包装器，提供了 io.LimitedReader 风格的限制（在读取请求头时）
	// 和支持 CloseNotifier 的功能。请参阅 *connReader 文档。
	r *connReader

	// bufr reads from r.
	// bufr 从 r 读取数据。
	bufr *bufio.Reader

	// bufw writes to checkConnErrorWriter{c}, which populates werr on error.
	// bufw 写入到 checkConnErrorWriter{c}，这会在发生错误时填充 werr。
	bufw *bufio.Writer

	// lastMethod is the method of the most recent request
	// on this connection, if any.
	// lastMethod 是此连接上最近一次请求的方法（如果存在）。
	lastMethod string

	curReq atomic.Pointer[response] // (which has a Request in it)
	// curReq 是一个指向 response 的原子指针 (其中包含一个 Request)。

	curState atomic.Uint64 // packed (unixtime<<8|uint8(ConnState))
	// curState 是一个原子 uint64，它被打包为 (unixtime<<8|uint8(ConnState))。

	// mu guards hijackedv
	// mu 保护 hijackedv。
	mu sync.Mutex

	// hijackedv is whether this connection has been hijacked
	// by a Handler with the Hijacker interface.
	// It is guarded by mu.
	// hijackedv 表示此连接是否已被具有 Hijacker 接口的 Handler 劫持。
	// 它由 mu 保护。
	hijackedv bool
}

func (c *conn) hijacked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hijackedv
}

// c.mu must be held.
func (c *conn) hijackLocked() (rwc net.Conn, buf *bufio.ReadWriter, err error) {
	// c.mu must be held.
	// c.mu 必须被持有.
	if c.hijackedv {
		// If the connection has already been hijacked, return an error.
		// 如果连接已经被劫持，返回一个错误。
		return nil, nil, ErrHijacked
	}
	c.r.abortPendingRead()
	// Abort any pending read operation. This is important to prevent
	// background reads from interfering with the hijacked connection.
	// 中止任何挂起的读取操作。这对于防止后台读取干扰被劫持的连接非常重要。

	c.hijackedv = true
	// Mark the connection as hijacked.
	// 标记连接为已劫持。
	rwc = c.rwc
	// Get the underlying network connection.
	// 获取底层网络连接。
	rwc.SetDeadline(time.Time{})
	// Clear any read/write deadlines on the connection.
	// 清除连接上的任何读取/写入期限。

	buf = bufio.NewReadWriter(c.bufr, bufio.NewWriter(rwc))
	// Create a new buffered read/writer using the existing buffered reader
	// and a new buffered writer that writes to the underlying connection.
	// 使用现有的缓冲读取器和一个新的缓冲写入器创建一个新的缓冲读取/写入器，该写入器写入到底层连接。
	if c.r.hasByte {
		// If the connReader has a buffered byte, peek at the buffered data
		// to ensure that the buffered reader is in a consistent state.
		// 如果 connReader 有一个缓冲字节，查看缓冲数据以确保缓冲读取器处于一致状态。
		if _, err := c.bufr.Peek(c.bufr.Buffered() + 1); err != nil {
			// If peeking fails, return an error.
			// 如果查看失败，则返回一个错误。
			return nil, nil, fmt.Errorf("unexpected Peek failure reading buffered byte: %v", err)
		}
	}
	c.setState(rwc, StateHijacked, runHooks)
	// Set the connection state to hijacked.
	// 将连接状态设置为已劫持。
	return
}

// This should be >= 512 bytes for DetectContentType,
// but otherwise it's somewhat arbitrary.
const bufferBeforeChunkingSize = 2048

// chunkWriter writes to a response's conn buffer, and is the writer
// wrapped by the response.w buffered writer.
//
// chunkWriter also is responsible for finalizing the Header, including
// conditionally setting the Content-Type and setting a Content-Length
// in cases where the handler's final output is smaller than the buffer
// size. It also conditionally adds chunk headers, when in chunking mode.
//
// See the comment above (*response).Write for the entire write flow.
// chunkWriter 将数据写入 response 的 conn buffer 中，它是 response.w buffered writer 包装的 writer。
// chunkWriter 还负责最终确定 Header，包括有条件地设置 Content-Type 和设置 Content-Length，
// 在某些情况下，handler 的最终输出小于缓冲区大小。当处于分块模式时，它还有条件地添加 chunk headers。
// 有关完整的写入流程，请参见上面的 (*response).Write 注释。
type chunkWriter struct {
	res *response
	// res is the response this chunkWriter is associated with.
	// res 是此 chunkWriter 与之关联的 response。

	// header is either nil or a deep clone of res.handlerHeader
	// at the time of res.writeHeader, if res.writeHeader is
	// called and extra buffering is being done to calculate
	// Content-Type and/or Content-Length.
	// header 要么是 nil，要么是 res.handlerHeader 的深层克隆，
	// 如果调用了 res.writeHeader 并且正在进行额外的缓冲以计算 Content-Type 和/或 Content-Length。
	header Header

	// wroteHeader tells whether the header's been written to "the
	// wire" (or rather: w.conn.buf). this is unlike
	// (*response).wroteHeader, which tells only whether it was
	// logically written.
	// wroteHeader 指示 header 是否已写入“the wire”（或者：w.conn.buf）。这与
	// (*response).wroteHeader 不同，后者仅指示它是否在逻辑上被写入。
	wroteHeader bool

	// set by the writeHeader method:
	// 由 writeHeader 方法设置：
	chunking bool // using chunked transfer encoding for reply body
	// chunking 表示是否使用分块传输编码来传输 reply body。
}

var (
	crlf       = []byte("\r\n")
	colonSpace = []byte(": ")
)

func (cw *chunkWriter) Write(p []byte) (n int, err error) {
	// Write writes len(p) bytes from p to the chunkWriter's buffer.
	// It returns the number of bytes written from p (0 <= n <= len(p))
	// and any error encountered that caused the write to stop early.
	// Write 在 chunkWriter 的缓冲区中写入 p 中的 len(p) 个字节。
	// 它返回从 p 写入的字节数 (0 <= n <= len(p)) 以及导致写入提前停止的任何错误。
	if !cw.wroteHeader {
		// If the header hasn't been written yet, write it now.
		// 如果 header 尚未写入，则立即写入。
		cw.writeHeader(p)
	}
	if cw.res.req.Method == "HEAD" {
		// For HEAD requests, discard the body.
		// 对于 HEAD 请求，丢弃 body。
		// Eat writes.
		return len(p), nil
	}
	if cw.chunking {
		// If chunking is enabled, write the chunk header.
		// 如果启用了 chunking，则写入 chunk header。
		_, err = fmt.Fprintf(cw.res.conn.bufw, "%x\r\n", len(p))
		if err != nil {
			// If there's an error writing the chunk header, close the connection and return.
			// 如果写入 chunk header 时发生错误，请关闭连接并返回。
			cw.res.conn.rwc.Close()
			return
		}
	}
	// Write the data to the buffer.
	// 将数据写入缓冲区。
	n, err = cw.res.conn.bufw.Write(p)
	if cw.chunking && err == nil {
		// If chunking is enabled and there's no error, write the chunk terminator.
		// 如果启用了 chunking 并且没有错误，则写入 chunk 终止符。
		_, err = cw.res.conn.bufw.Write(crlf)
	}
	if err != nil {
		// If there's an error writing the data or chunk terminator, close the connection.
		// 如果写入数据或 chunk 终止符时发生错误，请关闭连接。
		cw.res.conn.rwc.Close()
	}
	return
}

func (cw *chunkWriter) flush() error {
	if !cw.wroteHeader {
		cw.writeHeader(nil)
	}
	return cw.res.conn.bufw.Flush()
}

func (cw *chunkWriter) close() {
	// close finishes a chunked response.
	// close 完成一个 chunked 响应。
	if !cw.wroteHeader {
		// If the header hasn't been written yet, write it now.
		// 如果 header 尚未写入，则立即写入。
		cw.writeHeader(nil)
	}
	if cw.chunking {
		// If chunking is enabled, write the final chunk.
		// 如果启用了 chunking，则写入 final chunk。
		bw := cw.res.conn.bufw // conn's bufio writer  // 获取连接的 bufio writer
		// zero chunk to mark EOF  // 写入一个长度为 0 的 chunk，用于标记 EOF
		bw.WriteString("0\r\n")
		if trailers := cw.res.finalTrailers(); trailers != nil {
			// If there are trailers, write them.
			// 如果有 trailers，则写入它们。
			trailers.Write(bw) // the writer handles noting errors  // writer 会处理错误记录
		}
		// final blank line after the trailers (whether
		// present or not)
		// 在 trailers 之后添加 final blank line (无论是否存在 trailers)
		bw.WriteString("\r\n")
	}
}

// A response represents the server side of an HTTP response.
// response 表示 HTTP 响应的服务器端。
type response struct {
	conn             *conn
	req              *Request // request for this response  // 指向此响应的请求
	reqBody          io.ReadCloser
	cancelCtx        context.CancelFunc // when ServeHTTP exits  // ServeHTTP 退出时调用的取消函数
	wroteHeader      bool               // a non-1xx header has been (logically) written  // 是否已写入非 1xx 响应头（逻辑上）
	wants10KeepAlive bool               // HTTP/1.0 w/ Connection "keep-alive"  // 是否想要 HTTP/1.0 的 keep-alive 连接
	wantsClose       bool               // HTTP request has Connection "close"  // HTTP 请求是否包含 Connection "close"

	// canWriteContinue is an atomic boolean that says whether or
	// not a 100 Continue header can be written to the
	// connection.
	// canWriteContinue 是一个原子布尔值，表示是否可以将 100 Continue 标头写入连接。
	// writeContinueMu must be held while writing the header.
	// 在写入标头时，必须持有 writeContinueMu。
	// These two fields together synchronize the body reader (the
	// expectContinueReader, which wants to write 100 Continue)
	// against the main writer.
	// 这两个字段一起同步 body reader（expectContinueReader，它想要写入 100 Continue）和主 writer。
	writeContinueMu  sync.Mutex
	canWriteContinue atomic.Bool

	w  *bufio.Writer // buffers output in chunks to chunkWriter  // 用于缓冲输出的 bufio.Writer，以块的形式写入 chunkWriter
	cw chunkWriter

	// handlerHeader is the Header that Handlers get access to,
	// which may be retained and mutated even after WriteHeader.
	// handlerHeader 是 Handlers 可以访问的 Header，即使在 WriteHeader 之后也可以保留和修改。
	// handlerHeader is copied into cw.header at WriteHeader
	// time, and privately mutated thereafter.
	// handlerHeader 在 WriteHeader 时被复制到 cw.header 中，之后进行私有修改。
	handlerHeader Header
	calledHeader  bool // handler accessed handlerHeader via Header  // handler 是否通过 Header 访问了 handlerHeader

	written       int64 // number of bytes written in body  // 写入 body 的字节数
	contentLength int64 // explicitly-declared Content-Length; or -1  // 显式声明的 Content-Length；或 -1
	status        int   // status code passed to WriteHeader  // 传递给 WriteHeader 的状态码

	// close connection after this reply.  set on request and
	// updated after response from handler if there's a
	// "Connection: keep-alive" response header and a
	// Content-Length.
	// 在此回复后关闭连接。在请求时设置，并在处理程序的响应之后更新，如果存在 "Connection: keep-alive" 响应头和 Content-Length。
	closeAfterReply bool

	// When fullDuplex is false (the default), we consume any remaining
	// request body before starting to write a response.
	// 当 fullDuplex 为 false（默认值）时，在开始写入响应之前，我们会消耗掉所有剩余的请求 body。
	fullDuplex bool

	// requestBodyLimitHit is set by requestTooLarge when
	// maxBytesReader hits its max size. It is checked in
	// WriteHeader, to make sure we don't consume the
	// remaining request body to try to advance to the next HTTP
	// request. Instead, when this is set, we stop reading
	// subsequent requests on this connection and stop reading
	// input from it.
	// requestBodyLimitHit 由 requestTooLarge 设置，当 maxBytesReader 达到其最大大小时。
	// 它在 WriteHeader 中进行检查，以确保我们不会消耗剩余的请求 body，试图前进到下一个 HTTP 请求。
	// 相反，当设置此项时，我们停止在此连接上读取后续请求，并停止从中读取输入。
	requestBodyLimitHit bool

	// trailers are the headers to be sent after the handler
	// finishes writing the body. This field is initialized from
	// the Trailer response header when the response header is
	// written.
	// trailers 是在 handler 完成 body 写入后要发送的 header。
	// 此字段从写入 response header 时，从 Trailer 响应头初始化。
	trailers []string

	handlerDone atomic.Bool // set true when the handler exits  // 当 handler 退出时设置为 true

	// Buffers for Date, Content-Length, and status code
	// 用于 Date、Content-Length 和状态码的缓冲区
	dateBuf   [len(TimeFormat)]byte
	clenBuf   [10]byte
	statusBuf [3]byte

	// closeNotifyCh is the channel returned by CloseNotify.
	// TODO(bradfitz): this is currently (for Go 1.8) always
	// non-nil. Make this lazily-created again as it used to be?
	// closeNotifyCh 是由 CloseNotify 返回的 channel。
	// TODO(bradfitz): 这在 Go 1.8 中当前始终为非 nil。是否再次使其像以前一样延迟创建？
	closeNotifyCh  chan bool
	didCloseNotify atomic.Bool // atomic (only false->true winner should send)  // 原子操作 (只有 false->true 的胜出者应该发送)
}

func (c *response) SetReadDeadline(deadline time.Time) error {
	return c.conn.rwc.SetReadDeadline(deadline)
}

func (c *response) SetWriteDeadline(deadline time.Time) error {
	return c.conn.rwc.SetWriteDeadline(deadline)
}

func (c *response) EnableFullDuplex() error {
	c.fullDuplex = true
	return nil
}

// TrailerPrefix is a magic prefix for [ResponseWriter.Header] map keys
// that, if present, signals that the map entry is actually for
// the response trailers, and not the response headers. The prefix
// is stripped after the ServeHTTP call finishes and the values are
// sent in the trailers.
//
// This mechanism is intended only for trailers that are not known
// prior to the headers being written. If the set of trailers is fixed
// or known before the header is written, the normal Go trailers mechanism
// is preferred:
//
//	https://pkg.go.dev/net/http#ResponseWriter
//	https://pkg.go.dev/net/http#example-ResponseWriter-Trailers
//
// TrailerPrefix 是一个特殊的 magic prefix，用于 [ResponseWriter.Header] map 的 key。
// 如果存在该 prefix，则表示该 map entry 实际上是用于 response trailers，而不是 response headers。
// 在 ServeHTTP 调用结束后，该 prefix 会被移除，并且这些值会被发送到 trailers 中。
//
// 这种机制仅适用于在 headers 写入之前未知的 trailers。如果 trailers 的集合是固定的或在 header 写入之前已知的，
// 则首选使用标准的 Go trailers 机制：
//
//	https://pkg.go.dev/net/http#ResponseWriter
//	https://pkg.go.dev/net/http#example-ResponseWriter-Trailers
const TrailerPrefix = "Trailer:"

// finalTrailers is called after the Handler exits and returns a non-nil
// value if the Handler set any trailers.
// finalTrailers 在 Handler 退出后被调用，如果 Handler 设置了任何 trailers，则返回一个非 nil 值。
func (w *response) finalTrailers() Header {
	var t Header
	for k, vv := range w.handlerHeader {
		if kk, found := strings.CutPrefix(k, TrailerPrefix); found {
			// 如果 header 的 key 带有 TrailerPrefix 前缀，则表示这是一个 trailer
			if t == nil {
				t = make(Header)
			}
			t[kk] = vv // 将 trailer 的 key 和 value 添加到 t 中
		}
	}
	for _, k := range w.trailers {
		// 遍历 response 声明的 trailers
		if t == nil {
			t = make(Header)
		}
		for _, v := range w.handlerHeader[k] {
			// 将 response header 中声明的 trailer 添加到 t 中
			t.Add(k, v)
		}
	}
	return t
}

// declareTrailer is called for each Trailer header when the
// response header is written. It notes that a header will need to be
// written in the trailers at the end of the response.
// declareTrailer 在 response header 被写入时，为每个 Trailer header 调用。
// 它记录了一个 header 需要在 response 的 trailers 中被写入。
func (w *response) declareTrailer(k string) {
	k = CanonicalHeaderKey(k) // 规范化 header key
	if !httpguts.ValidTrailerHeader(k) {
		// Forbidden by RFC 7230, section 4.1.2
		// 根据 RFC 7230 第 4.1.2 节，这是被禁止的 trailer header
		return
	}
	w.trailers = append(w.trailers, k) // 将 trailer 的 key 添加到 response 的 trailers 列表中
}

// requestTooLarge is called by maxBytesReader when too much input has
// been read from the client.
// requestTooLarge 在从客户端读取了太多输入时，由 maxBytesReader 调用。
func (w *response) requestTooLarge() {
	w.closeAfterReply = true     // 标记为在回复后关闭连接
	w.requestBodyLimitHit = true // 标记为请求体大小超过限制
	if !w.wroteHeader {
		// 如果还没有写入 header
		w.Header().Set("Connection", "close") // 设置 Connection: close，通知客户端关闭连接
	}
}

// disableWriteContinue stops Request.Body.Read from sending an automatic 100-Continue.
// If a 100-Continue is being written, it waits for it to complete before continuing.
// disableWriteContinue 阻止 Request.Body.Read 发送自动的 100-Continue 响应。
// 如果正在写入 100-Continue 响应，它会等待其完成后再继续。
func (w *response) disableWriteContinue() {
	w.writeContinueMu.Lock()         // 获取互斥锁，保护 canWriteContinue 字段
	defer w.writeContinueMu.Unlock() // 函数返回前释放互斥锁

	w.canWriteContinue.Store(false) // 设置 canWriteContinue 为 false，表示禁止发送 100-Continue 响应
}

// writerOnly hides an io.Writer value's optional ReadFrom method
// from io.Copy.
// writerOnly 隐藏了 io.Writer 值的可选 ReadFrom 方法，使其不被 io.Copy 使用。
// This is a wrapper type that prevents io.Copy from using the ReadFrom
// method of the underlying io.Writer. This is used to force io.Copy to
// use the standard Read/Write methods, which are necessary for chunked
// encoding and other HTTP-specific behavior.
// 这是一个包装类型，阻止 io.Copy 使用底层 io.Writer 的 ReadFrom 方法。
// 这用于强制 io.Copy 使用标准的 Read/Write 方法，这对于分块编码和其他 HTTP 特定的行为是必要的。
type writerOnly struct {
	io.Writer
}

// ReadFrom is here to optimize copying from an [*os.File] regular file
// to a [*net.TCPConn] with sendfile, or from a supported src type such
// as a *net.TCPConn on Linux with splice.
// ReadFrom 用于优化从 [*os.File] 普通文件到使用 sendfile 的 [*net.TCPConn] 的复制，
// 或者从 Linux 上支持的源类型（如 *net.TCPConn）使用 splice 的复制。
func (w *response) ReadFrom(src io.Reader) (n int64, err error) {
	buf := getCopyBuf()   // 获取一个复制缓冲区
	defer putCopyBuf(buf) // 函数返回后，将复制缓冲区放回池中

	// Our underlying w.conn.rwc is usually a *TCPConn (with its
	// own ReadFrom method). If not, just fall back to the normal
	// copy method.
	// 我们的底层 w.conn.rwc 通常是一个 *TCPConn（它有自己的 ReadFrom 方法）。
	// 如果不是，则回退到正常的复制方法。
	rf, ok := w.conn.rwc.(io.ReaderFrom) // 尝试将 w.conn.rwc 断言为 io.ReaderFrom
	if !ok {
		return io.CopyBuffer(writerOnly{w}, src, buf) // 如果不是 io.ReaderFrom，则使用 io.CopyBuffer 进行复制
	}

	// Copy the first sniffLen bytes before switching to ReadFrom.
	// This ensures we don't start writing the response before the
	// source is available (see golang.org/issue/5660) and provides
	// enough bytes to perform Content-Type sniffing when required.
	// 在切换到 ReadFrom 之前，先复制前 sniffLen 个字节。
	// 这确保了我们不会在源可用之前开始写入响应（参见 golang.org/issue/5660），
	// 并提供了足够的字节来在需要时执行 Content-Type sniffing。
	if !w.cw.wroteHeader {
		n0, err := io.CopyBuffer(writerOnly{w}, io.LimitReader(src, sniffLen), buf) // 复制前 sniffLen 个字节
		n += n0                                                                     // 累加复制的字节数
		if err != nil || n0 < sniffLen {
			return n, err // 如果发生错误或复制的字节数小于 sniffLen，则返回
		}
	}

	w.w.Flush()  // get rid of any previous writes
	w.cw.flush() // make sure Header is written; flush data to rwc
	// w.w.Flush() 用于清除任何先前的写入
	// w.cw.flush() 确保 Header 已写入；将数据刷新到 rwc

	// Now that cw has been flushed, its chunking field is guaranteed initialized.
	// 现在 cw 已经被刷新，它的 chunking 字段保证被初始化。
	if !w.cw.chunking && w.bodyAllowed() {
		n0, err := rf.ReadFrom(src) // 使用 ReadFrom 进行复制
		n += n0                     // 累加复制的字节数
		w.written += n0             // 累加写入的字节数
		return n, err               // 返回
	}

	n0, err := io.CopyBuffer(writerOnly{w}, src, buf) // 如果不支持 ReadFrom 或启用了 chunking，则使用 io.CopyBuffer 进行复制
	n += n0                                           // 累加复制的字节数
	return n, err                                     // 返回
}

// debugServerConnections controls whether all server connections are wrapped
// with a verbose logging wrapper.
// debugServerConnections 控制是否所有服务器连接都使用详细的日志记录包装器进行包装。
const debugServerConnections = false

// Create new connection from rwc.
// newConn creates and initializes a new connection from the provided net.Conn.
// newConn 从提供的 net.Conn 创建并初始化一个新的连接。
func (srv *Server) newConn(rwc net.Conn) *conn {
	c := &conn{
		server: srv,
		rwc:    rwc,
	}
	if debugServerConnections {
		c.rwc = newLoggingConn("server", c.rwc)
	}
	return c
}

type readResult struct {
	_   incomparable
	n   int
	err error
	b   byte // byte read, if n == 1
}

// connReader is the io.Reader wrapper used by *conn. It combines a
// selectively-activated io.LimitedReader (to bound request header
// read sizes) with support for selectively keeping an io.Reader.Read
// call blocked in a background goroutine to wait for activity and
// trigger a CloseNotifier channel.
// connReader 是 *conn 使用的 io.Reader 包装器。它结合了选择性激活的 io.LimitedReader（用于限制请求头读取大小），
// 以及支持选择性地将 io.Reader.Read 调用阻塞在后台 goroutine 中，以等待活动并触发 CloseNotifier 通道。
type connReader struct {
	conn *conn // The connection this reader is associated with. 与此读取器关联的连接。

	mu      sync.Mutex // guards following 保护以下字段
	hasByte bool       // whether we have a byte already read 是否已经读取了一个字节
	byteBuf [1]byte    // single byte buffer for background reads 用于后台读取的单字节缓冲区
	cond    *sync.Cond // for waiting for background reads 用于等待后台读取
	inRead  bool       // whether a Read is currently in progress 是否当前正在进行读取操作
	aborted bool       // set true before conn.rwc deadline is set to past 在 conn.rwc 截止时间设置为过去之前设置为 true
	remain  int64      // bytes remaining 剩余的字节数
}

func (cr *connReader) lock() {
	cr.mu.Lock()
	if cr.cond == nil {
		cr.cond = sync.NewCond(&cr.mu)
	}
}

func (cr *connReader) unlock() { cr.mu.Unlock() }

func (cr *connReader) startBackgroundRead() {
	cr.lock()
	defer cr.unlock()
	// If a Read is already in progress, it's an invalid concurrent Body.Read call.
	// 如果已经在进行读取操作，则这是一个无效的并发 Body.Read 调用。
	if cr.inRead {
		panic("invalid concurrent Body.Read call")
	}
	// If we already have a byte, no need to start a background read.
	// 如果我们已经有一个字节，则无需启动后台读取。
	if cr.hasByte {
		return
	}
	// Mark that a Read is now in progress.
	// 标记现在正在进行读取操作。
	cr.inRead = true
	// Clear the read deadline.
	// 清除读取截止时间。
	cr.conn.rwc.SetReadDeadline(time.Time{})
	// Start the background read.
	// 启动后台读取。
	go cr.backgroundRead()
}

func (cr *connReader) backgroundRead() {
	// Reads a single byte from the connection in the background.
	// 在后台从连接中读取单个字节。
	n, err := cr.conn.rwc.Read(cr.byteBuf[:])
	cr.lock()
	if n == 1 {
		cr.hasByte = true
		// We were past the end of the previous request's body already
		// (since we wouldn't be in a background read otherwise), so
		// this is a pipelined HTTP request. Prior to Go 1.11 we used to
		// send on the CloseNotify channel and cancel the context here,
		// but the behavior was documented as only "may", and we only
		// did that because that's how CloseNotify accidentally behaved
		// in very early Go releases prior to context support. Once we
		// added context support, people used a Handler's
		// Request.Context() and passed it along. Having that context
		// cancel on pipelined HTTP requests caused problems.
		// Fortunately, almost nothing uses HTTP/1.x pipelining.
		// Unfortunately, apt-get does, or sometimes does.
		// New Go 1.11 behavior: don't fire CloseNotify or cancel
		// contexts on pipelined requests. Shouldn't affect people, but
		// fixes cases like Issue 23921. This does mean that a client
		// closing their TCP connection after sending a pipelined
		// request won't cancel the context, but we'll catch that on any
		// write failure (in checkConnErrorWriter.Write).
		// If the server never writes, yes, there are still contrived
		// server & client behaviors where this fails to ever cancel the
		// context, but that's kinda why HTTP/1.x pipelining died
		// anyway.
		//
		// 如果我们已经超过了前一个请求体的末尾（否则我们不会在后台读取中），那么这是一个管道化的 HTTP 请求。
		// 在 Go 1.11 之前，我们曾经在 CloseNotify 通道上发送并在此处取消上下文，但该行为被记录为仅“可能”，
		// 并且我们这样做只是因为那是 CloseNotify 在早期 Go 版本中意外表现的方式，早于上下文支持。
		// 一旦我们添加了上下文支持，人们就使用了 Handler 的 Request.Context() 并将其传递下去。
		// 在管道化的 HTTP 请求上取消该上下文会导致问题。
		// 幸运的是，几乎没有使用 HTTP/1.x 管道。
		// 不幸的是，apt-get 确实会这样做，或者有时会这样做。
		// 新的 Go 1.11 行为：不要在管道化的请求上触发 CloseNotify 或取消上下文。
		// 不应该影响人们，但修复了 Issue 23921 之类的问题。
		// 这确实意味着客户端在发送管道化请求后关闭其 TCP 连接不会取消上下文，
		// 但我们会在任何写入失败时捕获到这一点（在 checkConnErrorWriter.Write 中）。
		// 如果服务器从不写入，是的，仍然存在人为的服务器和客户端行为，导致无法取消上下文，
		// 但这就是 HTTP/1.x 管道无论如何都会消亡的原因。
	}
	if ne, ok := err.(net.Error); ok && cr.aborted && ne.Timeout() {
		// Ignore this error. It's the expected error from
		// another goroutine calling abortPendingRead.
		// 忽略此错误。这是来自另一个 goroutine 调用 abortPendingRead 的预期错误。
	} else if err != nil {
		cr.handleReadError(err)
	}
	cr.aborted = false
	cr.inRead = false
	cr.unlock()
	cr.cond.Broadcast()
}

func (cr *connReader) abortPendingRead() {
	cr.lock()
	defer cr.unlock()
	if !cr.inRead {
		return
	}
	cr.aborted = true                         // 设置 aborted 标志为 true，表示读取操作被中止。Set aborted flag to true, indicating the read operation is aborted.
	cr.conn.rwc.SetReadDeadline(aLongTimeAgo) // 设置读取截止时间为一个遥远的过去的时间，使得任何未完成的读取操作立即返回错误。Set read deadline to a long time ago, so any pending read operations will return an error immediately.
	for cr.inRead {                           // 循环等待，直到 inRead 标志变为 false，表示读取操作已经完成或被中断。Loop until inRead flag becomes false, indicating the read operation has completed or been interrupted.
		cr.cond.Wait() // 等待条件变量的通知。Wait for a notification on the condition variable.
	}
	cr.conn.rwc.SetReadDeadline(time.Time{}) // 重置读取截止时间为零值，取消截止时间限制。Reset read deadline to zero value, canceling the deadline restriction.
}

func (cr *connReader) setReadLimit(remain int64) { cr.remain = remain }
func (cr *connReader) setInfiniteReadLimit()     { cr.remain = maxInt64 }
func (cr *connReader) hitReadLimit() bool        { return cr.remain <= 0 }

// handleReadError is called whenever a Read from the client returns a
// non-nil error.
//
// The provided non-nil err is almost always io.EOF or a "use of
// closed network connection". In any case, the error is not
// particularly interesting, except perhaps for debugging during
// development. Any error means the connection is dead and we should
// down its context.
//
// It may be called from multiple goroutines.
// handleReadError 在从客户端的 Read 方法返回非 nil 错误时被调用。
// 提供的非 nil 错误几乎总是 io.EOF 或 "use of closed network connection"。
// 在任何情况下，该错误都不是特别重要，可能只在开发期间用于调试。
// 任何错误都意味着连接已断开，我们应该关闭其上下文。
// 可能会从多个 goroutine 调用它。
func (cr *connReader) handleReadError(_ error) {
	cr.conn.cancelCtx() // 取消与连接关联的上下文，通知所有监听该上下文的 goroutine 停止工作。Cancel the context associated with the connection, notifying all goroutines listening to the context to stop working.
	cr.closeNotify()    // 通知客户端连接即将关闭。Notify the client that the connection is about to close.
}

// may be called from multiple goroutines.
func (cr *connReader) closeNotify() {
	res := cr.conn.curReq.Load()
	if res != nil && !res.didCloseNotify.Swap(true) {
		res.closeNotifyCh <- true
	}
}

// Read 实现了 io.Reader 接口。
// Read 从连接中读取数据到 p 中。
// 它返回读取的字节数 (0 <= n <= len(p)) 和遇到的任何错误。
// 如果 Read 返回 n < len(p)，则它返回一个非 nil 错误。
// 如果 Read 返回 n == len(p)，则它返回一个 nil 错误。
// 如果 Read 返回 n == 0，则它返回一个 io.EOF 错误。
// 如果 Read 返回 n > 0，则它返回一个 nil 错误。
// 如果 Read 返回 n > 0，则它返回一个 nil 错误。
func (cr *connReader) Read(p []byte) (n int, err error) {
	// Read reads up to len(p) bytes into p. It returns the number of bytes
	// read (0 <= n <= len(p)) and any error encountered.
	// Read 从连接中读取最多 len(p) 字节的数据到 p 中。它返回读取的字节数 (0 <= n <= len(p)) 和遇到的任何错误。
	cr.lock()      // 获取 connReader 的锁，保证并发安全。Acquire the connReader's lock to ensure concurrency safety.
	if cr.inRead { // 检查是否已经在读取中。Check if a read operation is already in progress.
		cr.unlock()             // 释放锁，避免死锁。Release the lock to prevent deadlock.
		if cr.conn.hijacked() { // 如果连接已经被劫持。If the connection has been hijacked.
			panic("invalid Body.Read call. After hijacked, the original Request must not be used") // 抛出 panic，因为劫持后不应该再使用原始的 Request。Panic because the original Request should not be used after hijacking.
		}
		panic("invalid concurrent Body.Read call") // 抛出 panic，表示存在并发的 Body.Read 调用。Panic indicating a concurrent Body.Read call.
	}
	if cr.hitReadLimit() { // 检查是否达到读取限制。Check if the read limit has been reached.
		cr.unlock()      // 释放锁。Release the lock.
		return 0, io.EOF // 返回 io.EOF，表示读取结束。Return io.EOF, indicating the end of the read.
	}
	if len(p) == 0 { // 如果 p 的长度为 0。If the length of p is 0.
		cr.unlock()   // 释放锁。Release the lock.
		return 0, nil // 返回 0 和 nil，表示没有读取任何数据。Return 0 and nil, indicating that no data was read.
	}
	if int64(len(p)) > cr.remain { // 如果 p 的长度大于剩余可读取的字节数。If the length of p is greater than the remaining readable bytes.
		p = p[:cr.remain] // 截断 p，使其长度不超过剩余可读取的字节数。Truncate p so that its length does not exceed the remaining readable bytes.
	}
	if cr.hasByte { // 检查是否已经缓存了一个字节。Check if a byte has already been cached.
		p[0] = cr.byteBuf[0] // 将缓存的字节放入 p 的第一个字节。Put the cached byte into the first byte of p.
		cr.hasByte = false   // 清除 hasByte 标志。Clear the hasByte flag.
		cr.unlock()          // 释放锁。Release the lock.
		return 1, nil        // 返回 1 和 nil，表示读取了一个字节。Return 1 and nil, indicating that one byte was read.
	}
	cr.inRead = true             // 设置 inRead 标志为 true，表示正在进行读取操作。Set the inRead flag to true, indicating that a read operation is in progress.
	cr.unlock()                  // 释放锁。Release the lock.
	n, err = cr.conn.rwc.Read(p) // 从连接中读取数据到 p 中。Read data from the connection into p.

	cr.lock()         // 获取锁。Acquire the lock.
	cr.inRead = false // 设置 inRead 标志为 false，表示读取操作已经完成。Set the inRead flag to false, indicating that the read operation has completed.
	if err != nil {   // 如果发生错误。If an error occurred.
		cr.handleReadError(err) // 处理读取错误。Handle the read error.
	}
	cr.remain -= int64(n) // 减少剩余可读取的字节数。Reduce the number of remaining readable bytes.
	cr.unlock()           // 释放锁。Release the lock.

	cr.cond.Broadcast() // 广播条件变量，通知所有等待的 goroutine。Broadcast the condition variable, notifying all waiting goroutines.
	return n, err       // 返回读取的字节数和错误。Return the number of bytes read and the error.
}

var (
	bufioReaderPool   sync.Pool
	bufioWriter2kPool sync.Pool
	bufioWriter4kPool sync.Pool
)

const copyBufPoolSize = 32 * 1024

var copyBufPool = sync.Pool{New: func() any { return new([copyBufPoolSize]byte) }}

func getCopyBuf() []byte {
	return copyBufPool.Get().(*[copyBufPoolSize]byte)[:]
}
func putCopyBuf(b []byte) {
	if len(b) != copyBufPoolSize {
		panic("trying to put back buffer of the wrong size in the copyBufPool")
	}
	copyBufPool.Put((*[copyBufPoolSize]byte)(b))
}

func bufioWriterPool(size int) *sync.Pool {
	switch size {
	case 2 << 10:
		return &bufioWriter2kPool
	case 4 << 10:
		return &bufioWriter4kPool
	}
	return nil
}

// newBufioReader should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/gobwas/ws
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname newBufioReader
func newBufioReader(r io.Reader) *bufio.Reader {
	if v := bufioReaderPool.Get(); v != nil {
		br := v.(*bufio.Reader)
		br.Reset(r)
		return br
	}
	// Note: if this reader size is ever changed, update
	// TestHandlerBodyClose's assumptions.
	return bufio.NewReader(r)
}

// putBufioReader should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/gobwas/ws
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname putBufioReader
func putBufioReader(br *bufio.Reader) {
	br.Reset(nil)
	bufioReaderPool.Put(br)
}

// newBufioWriterSize should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/gobwas/ws
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname newBufioWriterSize
func newBufioWriterSize(w io.Writer, size int) *bufio.Writer {
	pool := bufioWriterPool(size)
	if pool != nil {
		if v := pool.Get(); v != nil {
			bw := v.(*bufio.Writer)
			bw.Reset(w)
			return bw
		}
	}
	return bufio.NewWriterSize(w, size)
}

// putBufioWriter should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/gobwas/ws
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname putBufioWriter
func putBufioWriter(bw *bufio.Writer) {
	bw.Reset(nil)
	if pool := bufioWriterPool(bw.Available()); pool != nil {
		pool.Put(bw)
	}
}

// DefaultMaxHeaderBytes is the maximum permitted size of the headers
// in an HTTP request.
// This can be overridden by setting [Server.MaxHeaderBytes].
const DefaultMaxHeaderBytes = 1 << 20 // 1 MB

func (srv *Server) maxHeaderBytes() int {
	if srv.MaxHeaderBytes > 0 {
		return srv.MaxHeaderBytes
	}
	return DefaultMaxHeaderBytes
}

func (srv *Server) initialReadLimitSize() int64 {
	return int64(srv.maxHeaderBytes()) + 4096 // bufio slop
}

// tlsHandshakeTimeout returns the time limit permitted for the TLS
// handshake, or zero for unlimited.
//
// It returns the minimum of any positive ReadHeaderTimeout,
// ReadTimeout, or WriteTimeout.
func (srv *Server) tlsHandshakeTimeout() time.Duration {
	var ret time.Duration
	for _, v := range [...]time.Duration{
		srv.ReadHeaderTimeout,
		srv.ReadTimeout,
		srv.WriteTimeout,
	} {
		if v <= 0 {
			continue
		}
		if ret == 0 || v < ret {
			ret = v
		}
	}
	return ret
}

// wrapper around io.ReadCloser which on first read, sends an
// HTTP/1.1 100 Continue header
type expectContinueReader struct {
	resp       *response
	readCloser io.ReadCloser
	closed     atomic.Bool
	sawEOF     atomic.Bool
}

func (ecr *expectContinueReader) Read(p []byte) (n int, err error) {
	if ecr.closed.Load() {
		return 0, ErrBodyReadAfterClose
	}
	w := ecr.resp
	if w.canWriteContinue.Load() {
		w.writeContinueMu.Lock()
		if w.canWriteContinue.Load() {
			w.conn.bufw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
			w.conn.bufw.Flush()
			w.canWriteContinue.Store(false)
		}
		w.writeContinueMu.Unlock()
	}
	n, err = ecr.readCloser.Read(p)
	if err == io.EOF {
		ecr.sawEOF.Store(true)
	}
	return
}

func (ecr *expectContinueReader) Close() error {
	ecr.closed.Store(true)
	return ecr.readCloser.Close()
}

// TimeFormat is the time format to use when generating times in HTTP
// headers. It is like [time.RFC1123] but hard-codes GMT as the time
// zone. The time being formatted must be in UTC for Format to
// generate the correct format.
//
// For parsing this time format, see [ParseTime].
const TimeFormat = "Mon, 02 Jan 2006 15:04:05 GMT"

// appendTime is a non-allocating version of []byte(t.UTC().Format(TimeFormat))
func appendTime(b []byte, t time.Time) []byte {
	const days = "SunMonTueWedThuFriSat"
	const months = "JanFebMarAprMayJunJulAugSepOctNovDec"

	t = t.UTC()
	yy, mm, dd := t.Date()
	hh, mn, ss := t.Clock()
	day := days[3*t.Weekday():]
	mon := months[3*(mm-1):]

	return append(b,
		day[0], day[1], day[2], ',', ' ',
		byte('0'+dd/10), byte('0'+dd%10), ' ',
		mon[0], mon[1], mon[2], ' ',
		byte('0'+yy/1000), byte('0'+(yy/100)%10), byte('0'+(yy/10)%10), byte('0'+yy%10), ' ',
		byte('0'+hh/10), byte('0'+hh%10), ':',
		byte('0'+mn/10), byte('0'+mn%10), ':',
		byte('0'+ss/10), byte('0'+ss%10), ' ',
		'G', 'M', 'T')
}

var errTooLarge = errors.New("http: request too large")

// Read next request from connection.
func (c *conn) readRequest(ctx context.Context) (w *response, err error) {
	if c.hijacked() {
		return nil, ErrHijacked
	}

	var (
		wholeReqDeadline time.Time // or zero if none
		hdrDeadline      time.Time // or zero if none
	)
	t0 := time.Now()
	if d := c.server.readHeaderTimeout(); d > 0 {
		hdrDeadline = t0.Add(d)
	}
	if d := c.server.ReadTimeout; d > 0 {
		wholeReqDeadline = t0.Add(d)
	}
	c.rwc.SetReadDeadline(hdrDeadline)
	if d := c.server.WriteTimeout; d > 0 {
		defer func() {
			c.rwc.SetWriteDeadline(time.Now().Add(d))
		}()
	}

	c.r.setReadLimit(c.server.initialReadLimitSize())
	if c.lastMethod == "POST" {
		// RFC 7230 section 3 tolerance for old buggy clients.
		peek, _ := c.bufr.Peek(4) // ReadRequest will get err below
		c.bufr.Discard(numLeadingCRorLF(peek))
	}
	req, err := readRequest(c.bufr)
	if err != nil {
		if c.r.hitReadLimit() {
			return nil, errTooLarge
		}
		return nil, err
	}

	if !http1ServerSupportsRequest(req) {
		return nil, statusError{StatusHTTPVersionNotSupported, "unsupported protocol version"}
	}

	c.lastMethod = req.Method
	c.r.setInfiniteReadLimit()

	hosts, haveHost := req.Header["Host"]
	isH2Upgrade := req.isH2Upgrade()
	if req.ProtoAtLeast(1, 1) && (!haveHost || len(hosts) == 0) && !isH2Upgrade && req.Method != "CONNECT" {
		return nil, badRequestError("missing required Host header")
	}
	if len(hosts) == 1 && !httpguts.ValidHostHeader(hosts[0]) {
		return nil, badRequestError("malformed Host header")
	}
	for k, vv := range req.Header {
		if !httpguts.ValidHeaderFieldName(k) {
			return nil, badRequestError("invalid header name")
		}
		for _, v := range vv {
			if !httpguts.ValidHeaderFieldValue(v) {
				return nil, badRequestError("invalid header value")
			}
		}
	}
	delete(req.Header, "Host")

	ctx, cancelCtx := context.WithCancel(ctx)
	req.ctx = ctx
	req.RemoteAddr = c.remoteAddr
	req.TLS = c.tlsState
	if body, ok := req.Body.(*body); ok {
		body.doEarlyClose = true
	}

	// Adjust the read deadline if necessary.
	if !hdrDeadline.Equal(wholeReqDeadline) {
		c.rwc.SetReadDeadline(wholeReqDeadline)
	}

	w = &response{
		conn:          c,
		cancelCtx:     cancelCtx,
		req:           req,
		reqBody:       req.Body,
		handlerHeader: make(Header),
		contentLength: -1,
		closeNotifyCh: make(chan bool, 1),

		// We populate these ahead of time so we're not
		// reading from req.Header after their Handler starts
		// and maybe mutates it (Issue 14940)
		wants10KeepAlive: req.wantsHttp10KeepAlive(),
		wantsClose:       req.wantsClose(),
	}
	if isH2Upgrade {
		w.closeAfterReply = true
	}
	w.cw.res = w
	w.w = newBufioWriterSize(&w.cw, bufferBeforeChunkingSize)
	return w, nil
}

// http1ServerSupportsRequest reports whether Go's HTTP/1.x server
// supports the given request.
func http1ServerSupportsRequest(req *Request) bool {
	if req.ProtoMajor == 1 {
		return true
	}
	// Accept "PRI * HTTP/2.0" upgrade requests, so Handlers can
	// wire up their own HTTP/2 upgrades.
	if req.ProtoMajor == 2 && req.ProtoMinor == 0 &&
		req.Method == "PRI" && req.RequestURI == "*" {
		return true
	}
	// Reject HTTP/0.x, and all other HTTP/2+ requests (which
	// aren't encoded in ASCII anyway).
	return false
}

func (w *response) Header() Header {
	if w.cw.header == nil && w.wroteHeader && !w.cw.wroteHeader {
		// Accessing the header between logically writing it
		// and physically writing it means we need to allocate
		// a clone to snapshot the logically written state.
		w.cw.header = w.handlerHeader.Clone()
	}
	w.calledHeader = true
	return w.handlerHeader
}

// maxPostHandlerReadBytes is the max number of Request.Body bytes not
// consumed by a handler that the server will read from the client
// in order to keep a connection alive. If there are more bytes
// than this, the server, to be paranoid, instead sends a
// "Connection close" response.
//
// This number is approximately what a typical machine's TCP buffer
// size is anyway.  (if we have the bytes on the machine, we might as
// well read them)
const maxPostHandlerReadBytes = 256 << 10

func checkWriteHeaderCode(code int) {
	// Issue 22880: require valid WriteHeader status codes.
	// For now we only enforce that it's three digits.
	// In the future we might block things over 599 (600 and above aren't defined
	// at https://httpwg.org/specs/rfc7231.html#status.codes).
	// But for now any three digits.
	//
	// We used to send "HTTP/1.1 000 0" on the wire in responses but there's
	// no equivalent bogus thing we can realistically send in HTTP/2,
	// so we'll consistently panic instead and help people find their bugs
	// early. (We can't return an error from WriteHeader even if we wanted to.)
	if code < 100 || code > 999 {
		panic(fmt.Sprintf("invalid WriteHeader code %v", code))
	}
}

// relevantCaller searches the call stack for the first function outside of net/http.
// The purpose of this function is to provide more helpful error messages.
func relevantCaller() runtime.Frame {
	pc := make([]uintptr, 16)
	n := runtime.Callers(1, pc)
	frames := runtime.CallersFrames(pc[:n])
	var frame runtime.Frame
	for {
		frame, more := frames.Next()
		if !strings.HasPrefix(frame.Function, "net/http.") {
			return frame
		}
		if !more {
			break
		}
	}
	return frame
}

func (w *response) WriteHeader(code int) {
	if w.conn.hijacked() {
		caller := relevantCaller()
		w.conn.server.logf("http: response.WriteHeader on hijacked connection from %s (%s:%d)", caller.Function, path.Base(caller.File), caller.Line)
		return
	}
	if w.wroteHeader {
		caller := relevantCaller()
		w.conn.server.logf("http: superfluous response.WriteHeader call from %s (%s:%d)", caller.Function, path.Base(caller.File), caller.Line)
		return
	}
	checkWriteHeaderCode(code)

	if code < 101 || code > 199 {
		// Sending a 100 Continue or any non-1xx header disables the
		// automatically-sent 100 Continue from Request.Body.Read.
		w.disableWriteContinue()
	}

	// Handle informational headers.
	//
	// We shouldn't send any further headers after 101 Switching Protocols,
	// so it takes the non-informational path.
	if code >= 100 && code <= 199 && code != StatusSwitchingProtocols {
		writeStatusLine(w.conn.bufw, w.req.ProtoAtLeast(1, 1), code, w.statusBuf[:])

		// Per RFC 8297 we must not clear the current header map
		w.handlerHeader.WriteSubset(w.conn.bufw, excludedHeadersNoBody)
		w.conn.bufw.Write(crlf)
		w.conn.bufw.Flush()

		return
	}

	w.wroteHeader = true
	w.status = code

	if w.calledHeader && w.cw.header == nil {
		w.cw.header = w.handlerHeader.Clone()
	}

	if cl := w.handlerHeader.get("Content-Length"); cl != "" {
		v, err := strconv.ParseInt(cl, 10, 64)
		if err == nil && v >= 0 {
			w.contentLength = v
		} else {
			w.conn.server.logf("http: invalid Content-Length of %q", cl)
			w.handlerHeader.Del("Content-Length")
		}
	}
}

// extraHeader is the set of headers sometimes added by chunkWriter.writeHeader.
// This type is used to avoid extra allocations from cloning and/or populating
// the response Header map and all its 1-element slices.
type extraHeader struct {
	contentType      string
	connection       string
	transferEncoding string
	date             []byte // written if not nil
	contentLength    []byte // written if not nil
}

// Sorted the same as extraHeader.Write's loop.
var extraHeaderKeys = [][]byte{
	[]byte("Content-Type"),
	[]byte("Connection"),
	[]byte("Transfer-Encoding"),
}

var (
	headerContentLength = []byte("Content-Length: ")
	headerDate          = []byte("Date: ")
)

// Write writes the headers described in h to w.
//
// This method has a value receiver, despite the somewhat large size
// of h, because it prevents an allocation. The escape analysis isn't
// smart enough to realize this function doesn't mutate h.
func (h extraHeader) Write(w *bufio.Writer) {
	if h.date != nil {
		w.Write(headerDate)
		w.Write(h.date)
		w.Write(crlf)
	}
	if h.contentLength != nil {
		w.Write(headerContentLength)
		w.Write(h.contentLength)
		w.Write(crlf)
	}
	for i, v := range []string{h.contentType, h.connection, h.transferEncoding} {
		if v != "" {
			w.Write(extraHeaderKeys[i])
			w.Write(colonSpace)
			w.WriteString(v)
			w.Write(crlf)
		}
	}
}

// writeHeader finalizes the header sent to the client and writes it
// to cw.res.conn.bufw.
//
// p is not written by writeHeader, but is the first chunk of the body
// that will be written. It is sniffed for a Content-Type if none is
// set explicitly. It's also used to set the Content-Length, if the
// total body size was small and the handler has already finished
// running.
func (cw *chunkWriter) writeHeader(p []byte) {
	if cw.wroteHeader {
		return
	}
	cw.wroteHeader = true

	w := cw.res
	keepAlivesEnabled := w.conn.server.doKeepAlives()
	isHEAD := w.req.Method == "HEAD"

	// header is written out to w.conn.buf below. Depending on the
	// state of the handler, we either own the map or not. If we
	// don't own it, the exclude map is created lazily for
	// WriteSubset to remove headers. The setHeader struct holds
	// headers we need to add.
	header := cw.header
	owned := header != nil
	if !owned {
		header = w.handlerHeader
	}
	var excludeHeader map[string]bool
	delHeader := func(key string) {
		if owned {
			header.Del(key)
			return
		}
		if _, ok := header[key]; !ok {
			return
		}
		if excludeHeader == nil {
			excludeHeader = make(map[string]bool)
		}
		excludeHeader[key] = true
	}
	var setHeader extraHeader

	// Don't write out the fake "Trailer:foo" keys. See TrailerPrefix.
	trailers := false
	for k := range cw.header {
		if strings.HasPrefix(k, TrailerPrefix) {
			if excludeHeader == nil {
				excludeHeader = make(map[string]bool)
			}
			excludeHeader[k] = true
			trailers = true
		}
	}
	for _, v := range cw.header["Trailer"] {
		trailers = true
		foreachHeaderElement(v, cw.res.declareTrailer)
	}

	te := header.get("Transfer-Encoding")
	hasTE := te != ""

	// If the handler is done but never sent a Content-Length
	// response header and this is our first (and last) write, set
	// it, even to zero. This helps HTTP/1.0 clients keep their
	// "keep-alive" connections alive.
	// Exceptions: 304/204/1xx responses never get Content-Length, and if
	// it was a HEAD request, we don't know the difference between
	// 0 actual bytes and 0 bytes because the handler noticed it
	// was a HEAD request and chose not to write anything. So for
	// HEAD, the handler should either write the Content-Length or
	// write non-zero bytes. If it's actually 0 bytes and the
	// handler never looked at the Request.Method, we just don't
	// send a Content-Length header.
	// Further, we don't send an automatic Content-Length if they
	// set a Transfer-Encoding, because they're generally incompatible.
	if w.handlerDone.Load() && !trailers && !hasTE && bodyAllowedForStatus(w.status) && !header.has("Content-Length") && (!isHEAD || len(p) > 0) {
		w.contentLength = int64(len(p))
		setHeader.contentLength = strconv.AppendInt(cw.res.clenBuf[:0], int64(len(p)), 10)
	}

	// If this was an HTTP/1.0 request with keep-alive and we sent a
	// Content-Length back, we can make this a keep-alive response ...
	if w.wants10KeepAlive && keepAlivesEnabled {
		sentLength := header.get("Content-Length") != ""
		if sentLength && header.get("Connection") == "keep-alive" {
			w.closeAfterReply = false
		}
	}

	// Check for an explicit (and valid) Content-Length header.
	hasCL := w.contentLength != -1

	if w.wants10KeepAlive && (isHEAD || hasCL || !bodyAllowedForStatus(w.status)) {
		_, connectionHeaderSet := header["Connection"]
		if !connectionHeaderSet {
			setHeader.connection = "keep-alive"
		}
	} else if !w.req.ProtoAtLeast(1, 1) || w.wantsClose {
		w.closeAfterReply = true
	}

	if header.get("Connection") == "close" || !keepAlivesEnabled {
		w.closeAfterReply = true
	}

	// If the client wanted a 100-continue but we never sent it to
	// them (or, more strictly: we never finished reading their
	// request body), don't reuse this connection.
	//
	// This behavior was first added on the theory that we don't know
	// if the next bytes on the wire are going to be the remainder of
	// the request body or the subsequent request (see issue 11549),
	// but that's not correct: If we keep using the connection,
	// the client is required to send the request body whether we
	// asked for it or not.
	//
	// We probably do want to skip reusing the connection in most cases,
	// however. If the client is offering a large request body that we
	// don't intend to use, then it's better to close the connection
	// than to read the body. For now, assume that if we're sending
	// headers, the handler is done reading the body and we should
	// drop the connection if we haven't seen EOF.
	if ecr, ok := w.req.Body.(*expectContinueReader); ok && !ecr.sawEOF.Load() {
		w.closeAfterReply = true
	}

	// We do this by default because there are a number of clients that
	// send a full request before starting to read the response, and they
	// can deadlock if we start writing the response with unconsumed body
	// remaining. See Issue 15527 for some history.
	//
	// If full duplex mode has been enabled with ResponseController.EnableFullDuplex,
	// then leave the request body alone.
	//
	// We don't take this path when w.closeAfterReply is set.
	// We may not need to consume the request to get ready for the next one
	// (since we're closing the conn), but a client which sends a full request
	// before reading a response may deadlock in this case.
	// This behavior has been present since CL 5268043 (2011), however,
	// so it doesn't seem to be causing problems.
	if w.req.ContentLength != 0 && !w.closeAfterReply && !w.fullDuplex {
		var discard, tooBig bool

		switch bdy := w.req.Body.(type) {
		case *expectContinueReader:
			// We only get here if we have already fully consumed the request body
			// (see above).
		case *body:
			bdy.mu.Lock()
			switch {
			case bdy.closed:
				if !bdy.sawEOF {
					// Body was closed in handler with non-EOF error.
					w.closeAfterReply = true
				}
			case bdy.unreadDataSizeLocked() >= maxPostHandlerReadBytes:
				tooBig = true
			default:
				discard = true
			}
			bdy.mu.Unlock()
		default:
			discard = true
		}

		if discard {
			_, err := io.CopyN(io.Discard, w.reqBody, maxPostHandlerReadBytes+1)
			switch err {
			case nil:
				// There must be even more data left over.
				tooBig = true
			case ErrBodyReadAfterClose:
				// Body was already consumed and closed.
			case io.EOF:
				// The remaining body was just consumed, close it.
				err = w.reqBody.Close()
				if err != nil {
					w.closeAfterReply = true
				}
			default:
				// Some other kind of error occurred, like a read timeout, or
				// corrupt chunked encoding. In any case, whatever remains
				// on the wire must not be parsed as another HTTP request.
				w.closeAfterReply = true
			}
		}

		if tooBig {
			w.requestTooLarge()
			delHeader("Connection")
			setHeader.connection = "close"
		}
	}

	code := w.status
	if bodyAllowedForStatus(code) {
		// If no content type, apply sniffing algorithm to body.
		_, haveType := header["Content-Type"]

		// If the Content-Encoding was set and is non-blank,
		// we shouldn't sniff the body. See Issue 31753.
		ce := header.Get("Content-Encoding")
		hasCE := len(ce) > 0
		if !hasCE && !haveType && !hasTE && len(p) > 0 {
			setHeader.contentType = DetectContentType(p)
		}
	} else {
		for _, k := range suppressedHeaders(code) {
			delHeader(k)
		}
	}

	if !header.has("Date") {
		setHeader.date = appendTime(cw.res.dateBuf[:0], time.Now())
	}

	if hasCL && hasTE && te != "identity" {
		// TODO: return an error if WriteHeader gets a return parameter
		// For now just ignore the Content-Length.
		w.conn.server.logf("http: WriteHeader called with both Transfer-Encoding of %q and a Content-Length of %d",
			te, w.contentLength)
		delHeader("Content-Length")
		hasCL = false
	}

	if w.req.Method == "HEAD" || !bodyAllowedForStatus(code) || code == StatusNoContent {
		// Response has no body.
		delHeader("Transfer-Encoding")
	} else if hasCL {
		// Content-Length has been provided, so no chunking is to be done.
		delHeader("Transfer-Encoding")
	} else if w.req.ProtoAtLeast(1, 1) {
		// HTTP/1.1 or greater: Transfer-Encoding has been set to identity, and no
		// content-length has been provided. The connection must be closed after the
		// reply is written, and no chunking is to be done. This is the setup
		// recommended in the Server-Sent Events candidate recommendation 11,
		// section 8.
		if hasTE && te == "identity" {
			cw.chunking = false
			w.closeAfterReply = true
			delHeader("Transfer-Encoding")
		} else {
			// HTTP/1.1 or greater: use chunked transfer encoding
			// to avoid closing the connection at EOF.
			cw.chunking = true
			setHeader.transferEncoding = "chunked"
			if hasTE && te == "chunked" {
				// We will send the chunked Transfer-Encoding header later.
				delHeader("Transfer-Encoding")
			}
		}
	} else {
		// HTTP version < 1.1: cannot do chunked transfer
		// encoding and we don't know the Content-Length so
		// signal EOF by closing connection.
		w.closeAfterReply = true
		delHeader("Transfer-Encoding") // in case already set
	}

	// Cannot use Content-Length with non-identity Transfer-Encoding.
	if cw.chunking {
		delHeader("Content-Length")
	}
	if !w.req.ProtoAtLeast(1, 0) {
		return
	}

	// Only override the Connection header if it is not a successful
	// protocol switch response and if KeepAlives are not enabled.
	// See https://golang.org/issue/36381.
	delConnectionHeader := w.closeAfterReply &&
		(!keepAlivesEnabled || !hasToken(cw.header.get("Connection"), "close")) &&
		!isProtocolSwitchResponse(w.status, header)
	if delConnectionHeader {
		delHeader("Connection")
		if w.req.ProtoAtLeast(1, 1) {
			setHeader.connection = "close"
		}
	}

	writeStatusLine(w.conn.bufw, w.req.ProtoAtLeast(1, 1), code, w.statusBuf[:])
	cw.header.WriteSubset(w.conn.bufw, excludeHeader)
	setHeader.Write(w.conn.bufw)
	w.conn.bufw.Write(crlf)
}

// foreachHeaderElement splits v according to the "#rule" construction
// in RFC 7230 section 7 and calls fn for each non-empty element.
func foreachHeaderElement(v string, fn func(string)) {
	v = textproto.TrimString(v)
	if v == "" {
		return
	}
	if !strings.Contains(v, ",") {
		fn(v)
		return
	}
	for _, f := range strings.Split(v, ",") {
		if f = textproto.TrimString(f); f != "" {
			fn(f)
		}
	}
}

// writeStatusLine writes an HTTP/1.x Status-Line (RFC 7230 Section 3.1.2)
// to bw. is11 is whether the HTTP request is HTTP/1.1. false means HTTP/1.0.
// code is the response status code.
// scratch is an optional scratch buffer. If it has at least capacity 3, it's used.
func writeStatusLine(bw *bufio.Writer, is11 bool, code int, scratch []byte) {
	if is11 {
		bw.WriteString("HTTP/1.1 ")
	} else {
		bw.WriteString("HTTP/1.0 ")
	}
	if text := StatusText(code); text != "" {
		bw.Write(strconv.AppendInt(scratch[:0], int64(code), 10))
		bw.WriteByte(' ')
		bw.WriteString(text)
		bw.WriteString("\r\n")
	} else {
		// don't worry about performance
		fmt.Fprintf(bw, "%03d status code %d\r\n", code, code)
	}
}

// bodyAllowed reports whether a Write is allowed for this response type.
// It's illegal to call this before the header has been flushed.
func (w *response) bodyAllowed() bool {
	if !w.wroteHeader {
		panic("")
	}
	return bodyAllowedForStatus(w.status)
}

// The Life Of A Write is like this:
//
// Handler starts. No header has been sent. The handler can either
// write a header, or just start writing. Writing before sending a header
// sends an implicitly empty 200 OK header.
//
// If the handler didn't declare a Content-Length up front, we either
// go into chunking mode or, if the handler finishes running before
// the chunking buffer size, we compute a Content-Length and send that
// in the header instead.
//
// Likewise, if the handler didn't set a Content-Type, we sniff that
// from the initial chunk of output.
//
// The Writers are wired together like:
//
//  1. *response (the ResponseWriter) ->
//  2. (*response).w, a [*bufio.Writer] of bufferBeforeChunkingSize bytes ->
//  3. chunkWriter.Writer (whose writeHeader finalizes Content-Length/Type)
//     and which writes the chunk headers, if needed ->
//  4. conn.bufw, a *bufio.Writer of default (4kB) bytes, writing to ->
//  5. checkConnErrorWriter{c}, which notes any non-nil error on Write
//     and populates c.werr with it if so, but otherwise writes to ->
//  6. the rwc, the [net.Conn].
//
// TODO(bradfitz): short-circuit some of the buffering when the
// initial header contains both a Content-Type and Content-Length.
// Also short-circuit in (1) when the header's been sent and not in
// chunking mode, writing directly to (4) instead, if (2) has no
// buffered data. More generally, we could short-circuit from (1) to
// (3) even in chunking mode if the write size from (1) is over some
// threshold and nothing is in (2).  The answer might be mostly making
// bufferBeforeChunkingSize smaller and having bufio's fast-paths deal
// with this instead.
func (w *response) Write(data []byte) (n int, err error) {
	return w.write(len(data), data, "")
}

func (w *response) WriteString(data string) (n int, err error) {
	return w.write(len(data), nil, data)
}

// either dataB or dataS is non-zero.
func (w *response) write(lenData int, dataB []byte, dataS string) (n int, err error) {
	if w.conn.hijacked() {
		if lenData > 0 {
			caller := relevantCaller()
			w.conn.server.logf("http: response.Write on hijacked connection from %s (%s:%d)", caller.Function, path.Base(caller.File), caller.Line)
		}
		return 0, ErrHijacked
	}

	if w.canWriteContinue.Load() {
		// Body reader wants to write 100 Continue but hasn't yet. Tell it not to.
		w.disableWriteContinue()
	}

	if !w.wroteHeader {
		w.WriteHeader(StatusOK)
	}
	if lenData == 0 {
		return 0, nil
	}
	if !w.bodyAllowed() {
		return 0, ErrBodyNotAllowed
	}

	w.written += int64(lenData) // ignoring errors, for errorKludge
	if w.contentLength != -1 && w.written > w.contentLength {
		return 0, ErrContentLength
	}
	if dataB != nil {
		return w.w.Write(dataB)
	} else {
		return w.w.WriteString(dataS)
	}
}

func (w *response) finishRequest() {
	w.handlerDone.Store(true)

	if !w.wroteHeader {
		w.WriteHeader(StatusOK)
	}

	w.w.Flush()
	putBufioWriter(w.w)
	w.cw.close()
	w.conn.bufw.Flush()

	w.conn.r.abortPendingRead()

	// Close the body (regardless of w.closeAfterReply) so we can
	// re-use its bufio.Reader later safely.
	w.reqBody.Close()

	if w.req.MultipartForm != nil {
		w.req.MultipartForm.RemoveAll()
	}
}

// shouldReuseConnection reports whether the underlying TCP connection can be reused.
// It must only be called after the handler is done executing.
func (w *response) shouldReuseConnection() bool {
	if w.closeAfterReply {
		// The request or something set while executing the
		// handler indicated we shouldn't reuse this
		// connection.
		return false
	}

	if w.req.Method != "HEAD" && w.contentLength != -1 && w.bodyAllowed() && w.contentLength != w.written {
		// Did not write enough. Avoid getting out of sync.
		return false
	}

	// There was some error writing to the underlying connection
	// during the request, so don't re-use this conn.
	if w.conn.werr != nil {
		return false
	}

	if w.closedRequestBodyEarly() {
		return false
	}

	return true
}

func (w *response) closedRequestBodyEarly() bool {
	body, ok := w.req.Body.(*body)
	return ok && body.didEarlyClose()
}

func (w *response) Flush() {
	w.FlushError()
}

func (w *response) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(StatusOK)
	}
	err := w.w.Flush()
	e2 := w.cw.flush()
	if err == nil {
		err = e2
	}
	return err
}

func (c *conn) finalFlush() {
	if c.bufr != nil {
		// Steal the bufio.Reader (~4KB worth of memory) and its associated
		// reader for a future connection.
		putBufioReader(c.bufr)
		c.bufr = nil
	}

	if c.bufw != nil {
		c.bufw.Flush()
		// Steal the bufio.Writer (~4KB worth of memory) and its associated
		// writer for a future connection.
		putBufioWriter(c.bufw)
		c.bufw = nil
	}
}

// Close the connection.
func (c *conn) close() {
	c.finalFlush()
	c.rwc.Close()
}

// rstAvoidanceDelay is the amount of time we sleep after closing the
// write side of a TCP connection before closing the entire socket.
// By sleeping, we increase the chances that the client sees our FIN
// and processes its final data before they process the subsequent RST
// from closing a connection with known unread data.
// This RST seems to occur mostly on BSD systems. (And Windows?)
// This timeout is somewhat arbitrary (~latency around the planet),
// and may be modified by tests.
//
// TODO(bcmills): This should arguably be a server configuration parameter,
// not a hard-coded value.
var rstAvoidanceDelay = 500 * time.Millisecond

type closeWriter interface {
	CloseWrite() error
}

var _ closeWriter = (*net.TCPConn)(nil)

// closeWriteAndWait flushes any outstanding data and sends a FIN packet (if
// client is connected via TCP), signaling that we're done. We then
// pause for a bit, hoping the client processes it before any
// subsequent RST.
//
// See https://golang.org/issue/3595
func (c *conn) closeWriteAndWait() {
	c.finalFlush()
	if tcp, ok := c.rwc.(closeWriter); ok {
		tcp.CloseWrite()
	}

	// When we return from closeWriteAndWait, the caller will fully close the
	// connection. If client is still writing to the connection, this will cause
	// the write to fail with ECONNRESET or similar. Unfortunately, many TCP
	// implementations will also drop unread packets from the client's read buffer
	// when a write fails, causing our final response to be truncated away too.
	//
	// As a result, https://www.rfc-editor.org/rfc/rfc7230#section-6.6 recommends
	// that “[t]he server … continues to read from the connection until it
	// receives a corresponding close by the client, or until the server is
	// reasonably certain that its own TCP stack has received the client's
	// acknowledgement of the packet(s) containing the server's last response.”
	//
	// Unfortunately, we have no straightforward way to be “reasonably certain”
	// that we have received the client's ACK, and at any rate we don't want to
	// allow a misbehaving client to soak up server connections indefinitely by
	// withholding an ACK, nor do we want to go through the complexity or overhead
	// of using low-level APIs to figure out when a TCP round-trip has completed.
	//
	// Instead, we declare that we are “reasonably certain” that we received the
	// ACK if maxRSTAvoidanceDelay has elapsed.
	time.Sleep(rstAvoidanceDelay)
}

// validNextProto reports whether the proto is a valid ALPN protocol name.
// Everything is valid except the empty string and built-in protocol types,
// so that those can't be overridden with alternate implementations.
func validNextProto(proto string) bool {
	switch proto {
	case "", "http/1.1", "http/1.0":
		return false
	}
	return true
}

const (
	runHooks  = true
	skipHooks = false
)

func (c *conn) setState(nc net.Conn, state ConnState, runHook bool) {
	srv := c.server
	switch state {
	case StateNew:
		srv.trackConn(c, true)
	case StateHijacked, StateClosed:
		srv.trackConn(c, false)
	}
	if state > 0xff || state < 0 {
		panic("internal error")
	}
	packedState := uint64(time.Now().Unix()<<8) | uint64(state)
	c.curState.Store(packedState)
	if !runHook {
		return
	}
	if hook := srv.ConnState; hook != nil {
		hook(nc, state)
	}
}

func (c *conn) getState() (state ConnState, unixSec int64) {
	packedState := c.curState.Load()
	return ConnState(packedState & 0xff), int64(packedState >> 8)
}

// badRequestError is a literal string (used by in the server in HTML,
// unescaped) to tell the user why their request was bad. It should
// be plain text without user info or other embedded errors.
func badRequestError(e string) error { return statusError{StatusBadRequest, e} }

// statusError is an error used to respond to a request with an HTTP status.
// The text should be plain text without user info or other embedded errors.
type statusError struct {
	code int
	text string
}

func (e statusError) Error() string { return StatusText(e.code) + ": " + e.text }

// ErrAbortHandler is a sentinel panic value to abort a handler.
// While any panic from ServeHTTP aborts the response to the client,
// panicking with ErrAbortHandler also suppresses logging of a stack
// trace to the server's error log.
var ErrAbortHandler = errors.New("net/http: abort Handler")

// isCommonNetReadError reports whether err is a common error
// encountered during reading a request off the network when the
// client has gone away or had its read fail somehow. This is used to
// determine which logs are interesting enough to log about.
func isCommonNetReadError(err error) bool {
	if err == io.EOF {
		return true
	}
	if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
		return true
	}
	if oe, ok := err.(*net.OpError); ok && oe.Op == "read" {
		return true
	}
	return false
}

// Serve a new connection.
func (c *conn) serve(ctx context.Context) {
	if ra := c.rwc.RemoteAddr(); ra != nil {
		c.remoteAddr = ra.String()
	}
	ctx = context.WithValue(ctx, LocalAddrContextKey, c.rwc.LocalAddr())
	var inFlightResponse *response
	defer func() {
		if err := recover(); err != nil && err != ErrAbortHandler {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			c.server.logf("http: panic serving %v: %v\n%s", c.remoteAddr, err, buf)
		}
		if inFlightResponse != nil {
			inFlightResponse.cancelCtx()
			inFlightResponse.disableWriteContinue()
		}
		if !c.hijacked() {
			if inFlightResponse != nil {
				inFlightResponse.conn.r.abortPendingRead()
				inFlightResponse.reqBody.Close()
			}
			c.close()
			c.setState(c.rwc, StateClosed, runHooks)
		}
	}()

	if tlsConn, ok := c.rwc.(*tls.Conn); ok {
		tlsTO := c.server.tlsHandshakeTimeout()
		if tlsTO > 0 {
			dl := time.Now().Add(tlsTO)
			c.rwc.SetReadDeadline(dl)
			c.rwc.SetWriteDeadline(dl)
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			// If the handshake failed due to the client not speaking
			// TLS, assume they're speaking plaintext HTTP and write a
			// 400 response on the TLS conn's underlying net.Conn.
			var reason string
			if re, ok := err.(tls.RecordHeaderError); ok && re.Conn != nil && tlsRecordHeaderLooksLikeHTTP(re.RecordHeader) {
				io.WriteString(re.Conn, "HTTP/1.0 400 Bad Request\r\n\r\nClient sent an HTTP request to an HTTPS server.\n")
				re.Conn.Close()
				reason = "client sent an HTTP request to an HTTPS server"
			} else {
				reason = err.Error()
			}
			c.server.logf("http: TLS handshake error from %s: %v", c.rwc.RemoteAddr(), reason)
			return
		}
		// Restore Conn-level deadlines.
		if tlsTO > 0 {
			c.rwc.SetReadDeadline(time.Time{})
			c.rwc.SetWriteDeadline(time.Time{})
		}
		c.tlsState = new(tls.ConnectionState)
		*c.tlsState = tlsConn.ConnectionState()
		if proto := c.tlsState.NegotiatedProtocol; validNextProto(proto) {
			if fn := c.server.TLSNextProto[proto]; fn != nil {
				h := initALPNRequest{ctx, tlsConn, serverHandler{c.server}}
				// Mark freshly created HTTP/2 as active and prevent any server state hooks
				// from being run on these connections. This prevents closeIdleConns from
				// closing such connections. See issue https://golang.org/issue/39776.
				c.setState(c.rwc, StateActive, skipHooks)
				fn(c.server, tlsConn, h)
			}
			return
		}
	}

	// HTTP/1.x from here on.

	ctx, cancelCtx := context.WithCancel(ctx)
	c.cancelCtx = cancelCtx
	defer cancelCtx()

	c.r = &connReader{conn: c}
	c.bufr = newBufioReader(c.r)
	c.bufw = newBufioWriterSize(checkConnErrorWriter{c}, 4<<10)

	for {
		w, err := c.readRequest(ctx)
		if c.r.remain != c.server.initialReadLimitSize() {
			// If we read any bytes off the wire, we're active.
			c.setState(c.rwc, StateActive, runHooks)
		}
		if err != nil {
			const errorHeaders = "\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n"

			switch {
			case err == errTooLarge:
				// Their HTTP client may or may not be
				// able to read this if we're
				// responding to them and hanging up
				// while they're still writing their
				// request. Undefined behavior.
				const publicErr = "431 Request Header Fields Too Large"
				fmt.Fprintf(c.rwc, "HTTP/1.1 "+publicErr+errorHeaders+publicErr)
				c.closeWriteAndWait()
				return

			case isUnsupportedTEError(err):
				// Respond as per RFC 7230 Section 3.3.1 which says,
				//      A server that receives a request message with a
				//      transfer coding it does not understand SHOULD
				//      respond with 501 (Unimplemented).
				code := StatusNotImplemented

				// We purposefully aren't echoing back the transfer-encoding's value,
				// so as to mitigate the risk of cross side scripting by an attacker.
				fmt.Fprintf(c.rwc, "HTTP/1.1 %d %s%sUnsupported transfer encoding", code, StatusText(code), errorHeaders)
				return

			case isCommonNetReadError(err):
				return // don't reply

			default:
				if v, ok := err.(statusError); ok {
					fmt.Fprintf(c.rwc, "HTTP/1.1 %d %s: %s%s%d %s: %s", v.code, StatusText(v.code), v.text, errorHeaders, v.code, StatusText(v.code), v.text)
					return
				}
				const publicErr = "400 Bad Request"
				fmt.Fprintf(c.rwc, "HTTP/1.1 "+publicErr+errorHeaders+publicErr)
				return
			}
		}

		// Expect 100 Continue support
		req := w.req
		if req.expectsContinue() {
			if req.ProtoAtLeast(1, 1) && req.ContentLength != 0 {
				// Wrap the Body reader with one that replies on the connection
				req.Body = &expectContinueReader{readCloser: req.Body, resp: w}
				w.canWriteContinue.Store(true)
			}
		} else if req.Header.get("Expect") != "" {
			w.sendExpectationFailed()
			return
		}

		c.curReq.Store(w)

		if requestBodyRemains(req.Body) {
			registerOnHitEOF(req.Body, w.conn.r.startBackgroundRead)
		} else {
			w.conn.r.startBackgroundRead()
		}

		// HTTP cannot have multiple simultaneous active requests.[*]
		// Until the server replies to this request, it can't read another,
		// so we might as well run the handler in this goroutine.
		// [*] Not strictly true: HTTP pipelining. We could let them all process
		// in parallel even if their responses need to be serialized.
		// But we're not going to implement HTTP pipelining because it
		// was never deployed in the wild and the answer is HTTP/2.
		inFlightResponse = w
		serverHandler{c.server}.ServeHTTP(w, w.req)
		inFlightResponse = nil
		w.cancelCtx()
		if c.hijacked() {
			return
		}
		w.finishRequest()
		c.rwc.SetWriteDeadline(time.Time{})
		if !w.shouldReuseConnection() {
			if w.requestBodyLimitHit || w.closedRequestBodyEarly() {
				c.closeWriteAndWait()
			}
			return
		}
		c.setState(c.rwc, StateIdle, runHooks)
		c.curReq.Store(nil)

		if !w.conn.server.doKeepAlives() {
			// We're in shutdown mode. We might've replied
			// to the user without "Connection: close" and
			// they might think they can send another
			// request, but such is life with HTTP/1.1.
			return
		}

		if d := c.server.idleTimeout(); d > 0 {
			c.rwc.SetReadDeadline(time.Now().Add(d))
		} else {
			c.rwc.SetReadDeadline(time.Time{})
		}

		// Wait for the connection to become readable again before trying to
		// read the next request. This prevents a ReadHeaderTimeout or
		// ReadTimeout from starting until the first bytes of the next request
		// have been received.
		if _, err := c.bufr.Peek(4); err != nil {
			return
		}

		c.rwc.SetReadDeadline(time.Time{})
	}
}

func (w *response) sendExpectationFailed() {
	// TODO(bradfitz): let ServeHTTP handlers handle
	// requests with non-standard expectation[s]? Seems
	// theoretical at best, and doesn't fit into the
	// current ServeHTTP model anyway. We'd need to
	// make the ResponseWriter an optional
	// "ExpectReplier" interface or something.
	//
	// For now we'll just obey RFC 7231 5.1.1 which says
	// "A server that receives an Expect field-value other
	// than 100-continue MAY respond with a 417 (Expectation
	// Failed) status code to indicate that the unexpected
	// expectation cannot be met."
	w.Header().Set("Connection", "close")
	w.WriteHeader(StatusExpectationFailed)
	w.finishRequest()
}

// Hijack implements the [Hijacker.Hijack] method. Our response is both a [ResponseWriter]
// and a [Hijacker].
func (w *response) Hijack() (rwc net.Conn, buf *bufio.ReadWriter, err error) {
	if w.handlerDone.Load() {
		panic("net/http: Hijack called after ServeHTTP finished")
	}
	w.disableWriteContinue()
	if w.wroteHeader {
		w.cw.flush()
	}

	c := w.conn
	c.mu.Lock()
	defer c.mu.Unlock()

	// Release the bufioWriter that writes to the chunk writer, it is not
	// used after a connection has been hijacked.
	rwc, buf, err = c.hijackLocked()
	if err == nil {
		putBufioWriter(w.w)
		w.w = nil
	}
	return rwc, buf, err
}

func (w *response) CloseNotify() <-chan bool {
	if w.handlerDone.Load() {
		panic("net/http: CloseNotify called after ServeHTTP finished")
	}
	return w.closeNotifyCh
}

func registerOnHitEOF(rc io.ReadCloser, fn func()) {
	switch v := rc.(type) {
	case *expectContinueReader:
		registerOnHitEOF(v.readCloser, fn)
	case *body:
		v.registerOnHitEOF(fn)
	default:
		panic("unexpected type " + fmt.Sprintf("%T", rc))
	}
}

// requestBodyRemains reports whether future calls to Read
// on rc might yield more data.
func requestBodyRemains(rc io.ReadCloser) bool {
	if rc == NoBody {
		return false
	}
	switch v := rc.(type) {
	case *expectContinueReader:
		return requestBodyRemains(v.readCloser)
	case *body:
		return v.bodyRemains()
	default:
		panic("unexpected type " + fmt.Sprintf("%T", rc))
	}
}

// The HandlerFunc type is an adapter to allow the use of
// ordinary functions as HTTP handlers. If f is a function
// with the appropriate signature, HandlerFunc(f) is a
// [Handler] that calls f.
type HandlerFunc func(ResponseWriter, *Request)

// ServeHTTP calls f(w, r).
func (f HandlerFunc) ServeHTTP(w ResponseWriter, r *Request) {
	f(w, r)
}

// Helper handlers

// Error replies to the request with the specified error message and HTTP code.
// It does not otherwise end the request; the caller should ensure no further
// writes are done to w.
// The error message should be plain text.
//
// Error deletes the Content-Length header,
// sets Content-Type to “text/plain; charset=utf-8”,
// and sets X-Content-Type-Options to “nosniff”.
// This configures the header properly for the error message,
// in case the caller had set it up expecting a successful output.
func Error(w ResponseWriter, error string, code int) {
	h := w.Header()

	// Delete the Content-Length header, which might be for some other content.
	// Assuming the error string fits in the writer's buffer, we'll figure
	// out the correct Content-Length for it later.
	//
	// We don't delete Content-Encoding, because some middleware sets
	// Content-Encoding: gzip and wraps the ResponseWriter to compress on-the-fly.
	// See https://go.dev/issue/66343.
	h.Del("Content-Length")

	// There might be content type already set, but we reset it to
	// text/plain for the error message.
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	fmt.Fprintln(w, error)
}

// NotFound replies to the request with an HTTP 404 not found error.
func NotFound(w ResponseWriter, r *Request) { Error(w, "404 page not found", StatusNotFound) }

// NotFoundHandler returns a simple request handler
// that replies to each request with a “404 page not found” reply.
func NotFoundHandler() Handler { return HandlerFunc(NotFound) }

// StripPrefix returns a handler that serves HTTP requests by removing the
// given prefix from the request URL's Path (and RawPath if set) and invoking
// the handler h. StripPrefix handles a request for a path that doesn't begin
// with prefix by replying with an HTTP 404 not found error. The prefix must
// match exactly: if the prefix in the request contains escaped characters
// the reply is also an HTTP 404 not found error.
func StripPrefix(prefix string, h Handler) Handler {
	if prefix == "" {
		return h
	}
	return HandlerFunc(func(w ResponseWriter, r *Request) {
		p := strings.TrimPrefix(r.URL.Path, prefix)
		rp := strings.TrimPrefix(r.URL.RawPath, prefix)
		if len(p) < len(r.URL.Path) && (r.URL.RawPath == "" || len(rp) < len(r.URL.RawPath)) {
			r2 := new(Request)
			*r2 = *r
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.Path = p
			r2.URL.RawPath = rp
			h.ServeHTTP(w, r2)
		} else {
			NotFound(w, r)
		}
	})
}

// Redirect replies to the request with a redirect to url,
// which may be a path relative to the request path.
//
// The provided code should be in the 3xx range and is usually
// [StatusMovedPermanently], [StatusFound] or [StatusSeeOther].
//
// If the Content-Type header has not been set, [Redirect] sets it
// to "text/html; charset=utf-8" and writes a small HTML body.
// Setting the Content-Type header to any value, including nil,
// disables that behavior.
func Redirect(w ResponseWriter, r *Request, url string, code int) {
	if u, err := urlpkg.Parse(url); err == nil {
		// If url was relative, make its path absolute by
		// combining with request path.
		// The client would probably do this for us,
		// but doing it ourselves is more reliable.
		// See RFC 7231, section 7.1.2
		if u.Scheme == "" && u.Host == "" {
			oldpath := r.URL.Path
			if oldpath == "" { // should not happen, but avoid a crash if it does
				oldpath = "/"
			}

			// no leading http://server
			if url == "" || url[0] != '/' {
				// make relative path absolute
				olddir, _ := path.Split(oldpath)
				url = olddir + url
			}

			var query string
			if i := strings.Index(url, "?"); i != -1 {
				url, query = url[:i], url[i:]
			}

			// clean up but preserve trailing slash
			trailing := strings.HasSuffix(url, "/")
			url = path.Clean(url)
			if trailing && !strings.HasSuffix(url, "/") {
				url += "/"
			}
			url += query
		}
	}

	h := w.Header()

	// RFC 7231 notes that a short HTML body is usually included in
	// the response because older user agents may not understand 301/307.
	// Do it only if the request didn't already have a Content-Type header.
	_, hadCT := h["Content-Type"]

	h.Set("Location", hexEscapeNonASCII(url))
	if !hadCT && (r.Method == "GET" || r.Method == "HEAD") {
		h.Set("Content-Type", "text/html; charset=utf-8")
	}
	w.WriteHeader(code)

	// Shouldn't send the body for POST or HEAD; that leaves GET.
	if !hadCT && r.Method == "GET" {
		body := "<a href=\"" + htmlEscape(url) + "\">" + StatusText(code) + "</a>.\n"
		fmt.Fprintln(w, body)
	}
}

var htmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	// "&#34;" is shorter than "&quot;".
	`"`, "&#34;",
	// "&#39;" is shorter than "&apos;" and apos was not in HTML until HTML5.
	"'", "&#39;",
)

func htmlEscape(s string) string {
	return htmlReplacer.Replace(s)
}

// Redirect to a fixed URL
type redirectHandler struct {
	url  string
	code int
}

func (rh *redirectHandler) ServeHTTP(w ResponseWriter, r *Request) {
	Redirect(w, r, rh.url, rh.code)
}

// RedirectHandler returns a request handler that redirects
// each request it receives to the given url using the given
// status code.
//
// The provided code should be in the 3xx range and is usually
// [StatusMovedPermanently], [StatusFound] or [StatusSeeOther].
func RedirectHandler(url string, code int) Handler {
	return &redirectHandler{url, code}
}

// ServeMux is an HTTP request multiplexer.
// ServeMux 是一个 HTTP 请求多路复用器。
// It matches the URL of each incoming request against a list of registered
// patterns and calls the handler for the pattern that
// most closely matches the URL.
// 它将每个传入请求的 URL 与一个已注册的模式列表进行匹配，并调用与 URL 最匹配的模式所对应的处理器。
//
// # Patterns
// # 模式
// ServeMux 支持多种模式匹配规则，允许根据请求的方法、主机和路径进行灵活的路由。
//
// Patterns can match the method, host and path of a request.
// 模式可以匹配请求的方法、主机和路径。
// Some examples:
// 一些示例：
//
//   - "/index.html" matches the path "/index.html" for any host and method.
//   - "/index.html"：匹配任何主机和任何方法，只要请求路径是 "/index.html"。
//   - "GET /static/" matches a GET request whose path begins with "/static/".
//   - "GET /static/"：匹配方法为 GET 且路径以 "/static/" 开头的请求。
//   - "example.com/" matches any request to the host "example.com".
//   - "example.com/"：匹配发送到主机 "example.com" 的任何请求，无论路径如何。
//   - "example.com/{$}" matches requests with host "example.com" and path "/".
//   - "example.com/{$}"：匹配主机为 "example.com" 且路径恰好是 "/" 的请求。这里的 `{$}` 是一个特殊通配符，表示路径的精确结束。
//   - "/b/{bucket}/o/{objectname...}" matches paths whose first segment is "b"
//     and whose third segment is "o". The name "bucket" denotes the second
//     segment and "objectname" denotes the remainder of the path.
//   - "/b/{bucket}/o/{objectname...}"：匹配路径的第一个段是 "b"、第三个段是 "o" 的路径。其中 "bucket" 表示路径的第二个段（一个通配符），"objectname" 表示路径的剩余部分（一个捕获所有后续段的通配符）。
//
// In general, a pattern looks like
// 通常，一个模式看起来像这样：
//
//	[METHOD ][HOST]/[PATH]
//
// All three parts are optional; "/" is a valid pattern.
// 这三部分都是可选的；"/" 是一个有效的模式。
// If METHOD is present, it must be followed by at least one space or tab.
// 如果 METHOD 存在，它后面必须至少跟一个空格或制表符。
//
// Literal (that is, non-wildcard) parts of a pattern match
// the corresponding parts of a request case-sensitively.
// 模式中的字面（即非通配符）部分与请求的相应部分进行大小写敏感的匹配。
//
// A pattern with no method matches every method. A pattern
// with the method GET matches both GET and HEAD requests.
// Otherwise, the method must match exactly.
// 没有指定方法的模式会匹配所有 HTTP 方法。
// 指定了 GET 方法的模式会同时匹配 GET 和 HEAD 请求（因为 HEAD 请求通常被视为 GET 请求的变体，只返回头部）。
// 除此之外，其他方法（如 POST, PUT, DELETE 等）必须精确匹配。
//
// A pattern with no host matches every host.
// 没有指定主机的模式会匹配所有主机。
// A pattern with a host matches URLs on that host only.
// 指定了主机的模式只匹配该主机上的 URL。
//
// A path can include wildcard segments of the form {NAME} or {NAME...}.
// 路径可以包含形如 {NAME} 或 {NAME...} 的通配符段。
// For example, "/b/{bucket}/o/{objectname...}".
// 例如，"/b/{bucket}/o/{objectname...}"。
// The wildcard name must be a valid Go identifier.
// 通配符的名称必须是有效的 Go 标识符（例如，不能包含特殊字符或以数字开头）。
// Wildcards must be full path segments: they must be preceded by a slash and followed by
// either a slash or the end of the string.
// 通配符必须是完整的路径段：它们必须以斜杠开头，并以斜杠或字符串的结尾结束。
// For example, "/b_{bucket}" is not a valid pattern.
// 例如，"/b_{bucket}" 不是一个有效的模式，因为通配符 `{bucket}` 没有被斜杠完全包围。
//
// Normally a wildcard matches only a single path segment,
// ending at the next literal slash (not %2F) in the request URL.
// 通常，一个通配符（如 {NAME}）只匹配单个路径段，在请求 URL 中遇到下一个字面斜杠（而不是编码后的 %2F）时结束匹配。
// But if the "..." is present, then the wildcard matches the remainder of the URL path, including slashes.
// 但如果存在 "..."（如 {NAME...}），则该通配符会匹配 URL 路径的剩余部分，包括其中的斜杠。
// (Therefore it is invalid for a "..." wildcard to appear anywhere but at the end of a pattern.)
// （因此，"..." 通配符不能出现在模式的末尾以外的任何位置，因为它会匹配所有剩余部分。）
// The match for a wildcard can be obtained by calling [Request.PathValue] with the wildcard's name.
// 通配符的匹配值可以通过调用 [Request.PathValue] 方法并传入通配符的名称来获取。
// A trailing slash in a path acts as an anonymous "..." wildcard.
// 路径末尾的斜杠（例如 "/foo/"）等同于一个匿名的 "..." 通配符，它会匹配以 "/foo/" 开头的所有路径。
//
// The special wildcard {$} matches only the end of the URL.
// 特殊通配符 {$} 只匹配 URL 的末尾。
// For example, the pattern "/{$}" matches only the path "/",
// 例如，模式 "/{$}" 只匹配路径 "/"。
// whereas the pattern "/" matches every path.
// 而模式 "/" 则匹配所有路径（包括 "/"、"/foo"、"/foo/bar" 等）。
//
// For matching, both pattern paths and incoming request paths are unescaped segment by segment.
// 为了进行匹配，模式路径和传入的请求路径都会逐段进行解码（unescaped）。
// So, for example, the path "/a%2Fb/100%25" is treated as having two segments, "a/b" and "100%".
// 因此，例如，路径 "/a%2Fb/100%25" 被视为包含两个段："a/b" 和 "100%"。
// The pattern "/a%2fb/" matches it, but the pattern "/a/b/" does not.
// 模式 "/a%2fb/" 可以匹配它（因为 %2f 解码后是斜杠，形成 "a/b" 段），但模式 "/a/b/" 则不能匹配（因为 "/a/b/" 会尝试匹配字面上的 "a" 和 "b" 段，而不是解码后的 "a/b"）。
//
// # Precedence
// # 优先级
//
// If two or more patterns match a request, then the most specific pattern takes precedence.
// 如果有两个或更多模式匹配一个请求，则最具体的模式（匹配范围最窄、最精确的模式）将优先。
// A pattern P1 is more specific than P2 if P1 matches a strict subset of P2’s requests;
// 如果模式 P1 匹配的请求集合是模式 P2 匹配的请求集合的严格子集，则 P1 比 P2 更具体；
// that is, if P2 matches all the requests of P1 and more.
// 也就是说，如果 P2 匹配了 P1 的所有请求，并且还匹配了 P1 未匹配到的其他请求，那么 P1 更具体。
// If neither is more specific, then the patterns conflict.
// 如果两个模式都没有比对方更具体（即它们匹配的请求集合存在交集但互不包含），则这些模式冲突。
// There is one exception to this rule, for backwards compatibility:
// 为了向后兼容，此规则有一个例外：
// if two patterns would otherwise conflict and one has a host while the other does not,
// 如果两个模式原本会冲突，但其中一个模式指定了主机（例如 "example.com/path"），而另一个模式没有指定主机（例如 "/path"），
// then the pattern with the host takes precedence.
// 则指定了主机的模式会优先。
// If a pattern passed to [ServeMux.Handle] or [ServeMux.HandleFunc] conflicts with
// 如果传递给 [ServeMux.Handle] 或 [ServeMux.HandleFunc] 的模式与
// another pattern that is already registered, those functions panic.
// 已经注册的其他模式发生冲突，则这些函数会引发 panic。
//
// As an example of the general rule, "/images/thumbnails/" is more specific than "/images/",
// 作为一般规则的一个例子，模式 "/images/thumbnails/" 比 "/images/" 更具体，
// so both can be registered.
// 因此两者可以同时注册。
// The former matches paths beginning with "/images/thumbnails/"
// 前者（"/images/thumbnails/"）会匹配所有以 "/images/thumbnails/" 开头的路径，
// and the latter will match any other path in the "/images/" subtree.
// 而后者（"/images/"）将匹配 "/images/" 子树中除前者匹配范围之外的其他路径。
//
// As another example, consider the patterns "GET /" and "/index.html":
// 另一个例子是，考虑模式 "GET /" 和 "/index.html"：
// both match a GET request for "/index.html", but the former pattern
// 它们都匹配对 "/index.html" 的 GET 请求，但前者（"GET /"）模式
// matches all other GET and HEAD requests, while the latter matches any
// 匹配所有其他 GET 和 HEAD 请求（例如 "GET /foo"），而后者（"/index.html"）匹配任何
// request for "/index.html" that uses a different method.
// 对 "/index.html" 使用不同方法（例如 POST /index.html）的请求。
// The patterns conflict.
// 这两个模式是冲突的，因为它们匹配的请求集合存在交集（GET /index.html）但互不包含。
//
// # Trailing-slash redirection
// # 尾部斜杠重定向
//
// Consider a [ServeMux] with a handler for a subtree, registered using a trailing slash or "..." wildcard.
// 考虑一个 [ServeMux]，它为一个子树注册了一个处理器，该子树的模式以尾部斜杠（例如 "/images/"）或 "..." 通配符（例如 "/files/..."）结尾。
// If the ServeMux receives a request for the subtree root without a trailing slash,
// it redirects the request by adding the trailing slash.
// 如果 ServeMux 收到一个针对该子树根路径的请求，但该请求没有尾部斜杠（例如 "/images"），
// 它会自动通过添加尾部斜杠的方式重定向该请求（例如重定向到 "/images/"）。
// This behavior can be overridden with a separate registration for the path without
// the trailing slash or "..." wildcard. For example, registering "/images/" causes ServeMux
// to redirect a request for "/images" to "/images/", unless "/images" has
// been registered separately.
// 这种重定向行为可以通过为不带尾部斜杠或 "..." 通配符的路径单独注册一个处理器来覆盖。
// 例如，注册模式 "/images/" 会导致 ServeMux 将对 "/images" 的请求重定向到 "/images/"，
// 除非你已经单独注册了 "/images" 这个精确路径的处理器。
//
// # Request sanitizing
// # 请求净化
//
// ServeMux also takes care of sanitizing the URL request path and the Host
// header, stripping the port number and redirecting any request containing . or
// .. segments or repeated slashes to an equivalent, cleaner URL.
// ServeMux 还会负责净化 URL 请求路径和 Host 头。
// 它会剥离 Host 头中的端口号，并将任何包含 "."（当前目录）或 ".."（上级目录）路径段，
// 或包含重复斜杠（例如 "//"）的请求重定向到一个等效的、更规范的 URL。
// 这种净化有助于提高安全性并确保路径匹配的一致性。
//
// # Compatibility
// # 兼容性
//
// The pattern syntax and matching behavior of ServeMux changed significantly
// in Go 1.22. To restore the old behavior, set the GODEBUG environment variable
// to "httpmuxgo121=1". This setting is read once, at program startup; changes
// during execution will be ignored.
// ServeMux 的模式语法和匹配行为在 Go 1.22 版本中发生了显著变化。
// 为了恢复到 Go 1.21 及以前版本的旧行为，可以将 GODEBUG 环境变量设置为 "httpmuxgo121=1"。
// 此设置在程序启动时只读取一次；在程序执行期间对该环境变量的任何更改都将被忽略，不会影响 ServeMux 的行为。
// 这为需要时间迁移到新行为的现有应用程序提供了向后兼容的选项。
//
// The backwards-incompatible changes include:
// 这些向后不兼容的更改包括：
//   - Wildcards are just ordinary literal path segments in 1.21.
//     For example, the pattern "/{x}" will match only that path in 1.21,
//     but will match any one-segment path in 1.22.
//   - 在 Go 1.21 中，通配符（如 `{x}`）被视为普通的字面路径段。
//     例如，模式 "/{x}" 在 1.21 中只会精确匹配路径 "/{x}"。
//     但在 Go 1.22 中，它会匹配任何由一个路径段组成的路径，例如 "/foo" 或 "/bar"，其中 "foo" 或 "bar" 会被捕获为 `{x}` 的值。
//   - In 1.21, no pattern was rejected, unless it was empty or conflicted with an existing pattern.
//     In 1.22, syntactically invalid patterns will cause [ServeMux.Handle] and [ServeMux.HandleFunc] to panic.
//     For example, in 1.21, the patterns "/{"  and "/a{x}" match themselves,
//     but in 1.22 they are invalid and will cause a panic when registered.
//   - 在 Go 1.21 中，除了空模式或与现有模式冲突的模式外，其他所有模式都不会被拒绝。
//     但在 Go 1.22 中，语法上无效的模式（例如不完整的通配符语法）将导致 [ServeMux.Handle] 和 [ServeMux.HandleFunc] 方法在注册时触发 panic。
//     例如，在 1.21 中，模式 "/{" 和 "/a{x}" 会被视为字面路径并匹配它们自身。
//     但在 1.22 中，这些模式被认为是无效的通配符语法，因此在注册时会引发 panic。
//   - In 1.22, each segment of a pattern is unescaped; this was not done in 1.21.
//     For example, in 1.22 the pattern "/%61" matches the path "/a" ("%61" being the URL escape sequence for "a"),
//     but in 1.21 it would match only the path "/%2561" (where "%25" is the escape for the percent sign).
//   - 在 Go 1.22 中，模式的每个路径段都会被 URL 解码（unescaped）；而在 1.21 中则没有这样做。
//     例如，在 1.22 中，模式 "/%61" 会匹配路径 "/a"（因为 "%61" 是字符 "a" 的 URL 编码）。
//     但在 1.21 中，它只会匹配路径 "/%2561"（因为 "%25" 是百分号的编码，所以 "%61" 被视为字面量，而不会被解码）。
//   - When matching patterns to paths, in 1.22 each segment of the path is unescaped; in 1.21, the entire path is unescaped.
//     This change mostly affects how paths with %2F escapes adjacent to slashes are treated.
//     See https://go.dev/issue/21955 for details.
//   - 在将模式与请求路径匹配时，Go 1.22 会对路径的每个段进行 URL 解码；而在 1.21 中，是对整个路径进行一次性解码。
//     这一变化主要影响了包含 "%2F"（斜杠的 URL 编码）且紧邻实际斜杠的路径的处理方式。
//     有关详细信息，请参阅 https://go.dev/issue/21955。
type ServeMux struct {
	mu sync.RWMutex // mu 是一个读写互斥锁，用于保护 ServeMux 的内部数据结构，确保并发访问时的线程安全。

	tree routingNode // tree 是一个路由节点树，用于存储和匹配注册的 URL 模式。它通常是一个前缀树（trie），用于高效地查找与传入请求路径匹配的处理器。

	index routingIndex // index 是一个路由索引，用于优化模式查找，特别是在处理基于主机名的模式时。它可以加速匹配过程。

	patterns []*pattern // patterns 存储所有已注册的模式列表。
	// patterns stores a list of all registered patterns.
	// TODO(jba): remove if possible // TODO(jba): 如果可能的话，移除此字段，因为它可能在新的路由实现中不再需要。
	// TODO(jba): remove if possible // TODO(jba): Remove this field if possible, as it might not be needed in the new routing implementation.

	mux121 serveMux121 // mux121 是一个内部字段，仅当 GODEBUG 环境变量设置为 "httpmuxgo121=1" 时使用。它提供了 Go 1.21 版本的 ServeMux 行为，用于向后兼容。
}

// NewServeMux allocates and returns a new [ServeMux].
func NewServeMux() *ServeMux {
	return &ServeMux{}
}

// DefaultServeMux is the default [ServeMux] used by [Serve].
var DefaultServeMux = &defaultServeMux

var defaultServeMux ServeMux

// cleanPath returns the canonical path for p, eliminating . and .. elements.
// cleanPath 函数返回路径 p 的规范形式，它会消除路径中的 "." 和 ".." 元素。
func cleanPath(p string) string {
	// If the path is empty, return the root path.
	// 如果路径为空，则返回根路径 "/"。
	if p == "" {
		return "/"
	}
	// If the path does not start with a slash, prepend one to make it an absolute path.
	// 如果路径不是以斜杠开头，则在前面添加一个斜杠，确保路径是绝对路径。
	if p[0] != '/' {
		p = "/" + p
	}
	// Use path.Clean to remove "." and ".." elements and normalize slashes.
	// 使用 path.Clean 清理路径，它会移除 "." 和 ".." 元素，并处理多余的斜杠。
	np := path.Clean(p)
	// path.Clean removes trailing slash except for root;
	// path.Clean 函数会移除路径末尾的斜杠，但根路径 "/" 除外。
	// put the trailing slash back if necessary.
	// 如果原始路径以斜杠结尾，并且清理后的路径不是根路径，则需要将末尾的斜杠添加回来。
	if p[len(p)-1] == '/' && np != "/" {
		// Fast path for common case of p being the string we want:
		// 针对常见情况的快速路径优化：如果原始路径 p 已经是我们想要的（即 np 加上一个斜杠），则直接使用 p。
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			// Otherwise, append a trailing slash to the cleaned path.
			// 否则，在清理后的路径 np 末尾添加一个斜杠。
			np += "/"
		}
	}
	// Return the canonicalized path.
	// 返回处理后的规范路径。
	return np
}

// stripHostPort returns h without any trailing ":<port>".
func stripHostPort(h string) string {
	// If no port on host, return unchanged
	if !strings.Contains(h, ":") {
		return h
	}
	host, _, err := net.SplitHostPort(h)
	if err != nil {
		return h // on error, return unchanged
	}
	return host
}

// Handler returns the handler to use for the given request,
// consulting r.Method, r.Host, and r.URL.Path. It always returns
// a non-nil handler. If the path is not in its canonical form, the
// handler will be an internally-generated handler that redirects
// to the canonical path. If the host contains a port, it is ignored
// when matching handlers.
//
// The path and host are used unchanged for CONNECT requests.
//
// Handler also returns the registered pattern that matches the
// request or, in the case of internally-generated redirects,
// the path that will match after following the redirect.
//
// If there is no registered handler that applies to the request,
// Handler returns a “page not found” handler and an empty pattern.
func (mux *ServeMux) Handler(r *Request) (h Handler, pattern string) {
	if use121 {
		return mux.mux121.findHandler(r)
	}
	h, p, _, _ := mux.findHandler(r)
	return h, p
}

// findHandler finds a handler for a request.
// If there is a matching handler, it returns it and the pattern that matched.
// Otherwise it returns a Redirect or NotFound handler with the path that would match
// after the redirect.
func (mux *ServeMux) findHandler(r *Request) (h Handler, patStr string, _ *pattern, matches []string) {
	var n *routingNode
	host := r.URL.Host
	escapedPath := r.URL.EscapedPath()
	path := escapedPath
	// CONNECT requests are not canonicalized.
	if r.Method == "CONNECT" {
		// If r.URL.Path is /tree and its handler is not registered,
		// the /tree -> /tree/ redirect applies to CONNECT requests
		// but the path canonicalization does not.
		_, _, u := mux.matchOrRedirect(host, r.Method, path, r.URL)
		if u != nil {
			return RedirectHandler(u.String(), StatusMovedPermanently), u.Path, nil, nil
		}
		// Redo the match, this time with r.Host instead of r.URL.Host.
		// Pass a nil URL to skip the trailing-slash redirect logic.
		n, matches, _ = mux.matchOrRedirect(r.Host, r.Method, path, nil)
	} else {
		// All other requests have any port stripped and path cleaned
		// before passing to mux.handler.
		host = stripHostPort(r.Host)
		path = cleanPath(path)

		// If the given path is /tree and its handler is not registered,
		// redirect for /tree/.
		var u *url.URL
		n, matches, u = mux.matchOrRedirect(host, r.Method, path, r.URL)
		if u != nil {
			return RedirectHandler(u.String(), StatusMovedPermanently), u.Path, nil, nil
		}
		if path != escapedPath {
			// Redirect to cleaned path.
			patStr := ""
			if n != nil {
				patStr = n.pattern.String()
			}
			u := &url.URL{Path: path, RawQuery: r.URL.RawQuery}
			return RedirectHandler(u.String(), StatusMovedPermanently), patStr, nil, nil
		}
	}
	if n == nil {
		// We didn't find a match with the request method. To distinguish between
		// Not Found and Method Not Allowed, see if there is another pattern that
		// matches except for the method.
		allowedMethods := mux.matchingMethods(host, path)
		if len(allowedMethods) > 0 {
			return HandlerFunc(func(w ResponseWriter, r *Request) {
				w.Header().Set("Allow", strings.Join(allowedMethods, ", "))
				Error(w, StatusText(StatusMethodNotAllowed), StatusMethodNotAllowed)
			}), "", nil, nil
		}
		return NotFoundHandler(), "", nil, nil
	}
	return n.handler, n.pattern.String(), n.pattern, matches
}

// matchOrRedirect looks up a node in the tree that matches the host, method and path.
//
// If the url argument is non-nil, handler also deals with trailing-slash
// redirection: when a path doesn't match exactly, the match is tried again
// after appending "/" to the path. If that second match succeeds, the last
// return value is the URL to redirect to.
func (mux *ServeMux) matchOrRedirect(host, method, path string, u *url.URL) (_ *routingNode, matches []string, redirectTo *url.URL) {
	mux.mu.RLock()
	defer mux.mu.RUnlock()

	n, matches := mux.tree.match(host, method, path)
	// If we have an exact match, or we were asked not to try trailing-slash redirection,
	// or the URL already has a trailing slash, then we're done.
	if !exactMatch(n, path) && u != nil && !strings.HasSuffix(path, "/") {
		// If there is an exact match with a trailing slash, then redirect.
		path += "/"
		n2, _ := mux.tree.match(host, method, path)
		if exactMatch(n2, path) {
			return nil, nil, &url.URL{Path: cleanPath(u.Path) + "/", RawQuery: u.RawQuery}
		}
	}
	return n, matches, nil
}

// exactMatch reports whether the node's pattern exactly matches the path.
// As a special case, if the node is nil, exactMatch return false.
//
// Before wildcards were introduced, it was clear that an exact match meant
// that the pattern and path were the same string. The only other possibility
// was that a trailing-slash pattern, like "/", matched a path longer than
// it, like "/a".
//
// With wildcards, we define an inexact match as any one where a multi wildcard
// matches a non-empty string. All other matches are exact.
// For example, these are all exact matches:
//
//	pattern   path
//	/a        /a
//	/{x}      /a
//	/a/{$}    /a/
//	/a/       /a/
//
// The last case has a multi wildcard (implicitly), but the match is exact because
// the wildcard matches the empty string.
//
// Examples of matches that are not exact:
//
//	pattern   path
//	/         /a
//	/a/{x...} /a/b
func exactMatch(n *routingNode, path string) bool {
	if n == nil {
		return false
	}
	// We can't directly implement the definition (empty match for multi
	// wildcard) because we don't record a match for anonymous multis.

	// If there is no multi, the match is exact.
	if !n.pattern.lastSegment().multi {
		return true
	}

	// If the path doesn't end in a trailing slash, then the multi match
	// is non-empty.
	if len(path) > 0 && path[len(path)-1] != '/' {
		return false
	}
	// Only patterns ending in {$} or a multi wildcard can
	// match a path with a trailing slash.
	// For the match to be exact, the number of pattern
	// segments should be the same as the number of slashes in the path.
	// E.g. "/a/b/{$}" and "/a/b/{...}" exactly match "/a/b/", but "/a/" does not.
	return len(n.pattern.segments) == strings.Count(path, "/")
}

// matchingMethods return a sorted list of all methods that would match with the given host and path.
func (mux *ServeMux) matchingMethods(host, path string) []string {
	// Hold the read lock for the entire method so that the two matches are done
	// on the same set of registered patterns.
	mux.mu.RLock()
	defer mux.mu.RUnlock()
	ms := map[string]bool{}
	mux.tree.matchingMethods(host, path, ms)
	// matchOrRedirect will try appending a trailing slash if there is no match.
	if !strings.HasSuffix(path, "/") {
		mux.tree.matchingMethods(host, path+"/", ms)
	}
	return slices.Sorted(maps.Keys(ms))
}

// ServeHTTP dispatches the request to the handler whose
// pattern most closely matches the request URL.
func (mux *ServeMux) ServeHTTP(w ResponseWriter, r *Request) {
	if r.RequestURI == "*" {
		if r.ProtoAtLeast(1, 1) {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(StatusBadRequest)
		return
	}
	var h Handler
	if use121 {
		h, _ = mux.mux121.findHandler(r)
	} else {
		h, r.Pattern, r.pat, r.matches = mux.findHandler(r)
	}
	h.ServeHTTP(w, r)
}

// The four functions below all call ServeMux.register so that callerLocation
// always refers to user code.

// Handle registers the handler for the given pattern.
// If the given pattern conflicts, with one that is already registered, Handle
// panics.
func (mux *ServeMux) Handle(pattern string, handler Handler) {
	if use121 {
		mux.mux121.handle(pattern, handler)
	} else {
		mux.register(pattern, handler)
	}
}

// HandleFunc registers the handler function for the given pattern.
// If the given pattern conflicts, with one that is already registered, HandleFunc
// panics.
func (mux *ServeMux) HandleFunc(pattern string, handler func(ResponseWriter, *Request)) {
	if use121 {
		mux.mux121.handleFunc(pattern, handler)
	} else {
		mux.register(pattern, HandlerFunc(handler))
	}
}

// Handle registers the handler for the given pattern in [DefaultServeMux].
// The documentation for [ServeMux] explains how patterns are matched.
func Handle(pattern string, handler Handler) {
	if use121 {
		DefaultServeMux.mux121.handle(pattern, handler)
	} else {
		DefaultServeMux.register(pattern, handler)
	}
}

// HandleFunc registers the handler function for the given pattern in [DefaultServeMux].
// The documentation for [ServeMux] explains how patterns are matched.
func HandleFunc(pattern string, handler func(ResponseWriter, *Request)) {
	if use121 {
		DefaultServeMux.mux121.handleFunc(pattern, handler)
	} else {
		DefaultServeMux.register(pattern, HandlerFunc(handler))
	}
}

func (mux *ServeMux) register(pattern string, handler Handler) {
	if err := mux.registerErr(pattern, handler); err != nil {
		panic(err)
	}
}

func (mux *ServeMux) registerErr(patstr string, handler Handler) error {
	if patstr == "" {
		return errors.New("http: invalid pattern")
	}
	if handler == nil {
		return errors.New("http: nil handler")
	}
	if f, ok := handler.(HandlerFunc); ok && f == nil {
		return errors.New("http: nil handler")
	}

	pat, err := parsePattern(patstr)
	if err != nil {
		return fmt.Errorf("parsing %q: %w", patstr, err)
	}

	// Get the caller's location, for better conflict error messages.
	// Skip register and whatever calls it.
	_, file, line, ok := runtime.Caller(3)
	if !ok {
		pat.loc = "unknown location"
	} else {
		pat.loc = fmt.Sprintf("%s:%d", file, line)
	}

	mux.mu.Lock()
	defer mux.mu.Unlock()
	// Check for conflict.
	if err := mux.index.possiblyConflictingPatterns(pat, func(pat2 *pattern) error {
		if pat.conflictsWith(pat2) {
			d := describeConflict(pat, pat2)
			return fmt.Errorf("pattern %q (registered at %s) conflicts with pattern %q (registered at %s):\n%s",
				pat, pat.loc, pat2, pat2.loc, d)
		}
		return nil
	}); err != nil {
		return err
	}
	mux.tree.addPattern(pat, handler)
	mux.index.addPattern(pat)
	mux.patterns = append(mux.patterns, pat)
	return nil
}

// Serve accepts incoming HTTP connections on the listener l,
// creating a new service goroutine for each. The service goroutines
// read requests and then call handler to reply to them.
//
// The handler is typically nil, in which case [DefaultServeMux] is used.
//
// HTTP/2 support is only enabled if the Listener returns [*tls.Conn]
// connections and they were configured with "h2" in the TLS
// Config.NextProtos.
//
// Serve always returns a non-nil error.
// Serve 接受监听器 l 上的传入 HTTP 连接，为每个连接创建一个新的服务 goroutine。
// 这些服务 goroutine 读取请求，然后调用 handler 来回复它们。
//
// handler 通常为 nil，在这种情况下，将使用 DefaultServeMux。
//
// 只有当 Listener 返回 [*tls.Conn] 连接，并且这些连接在 TLS Config.NextProtos 中配置了 "h2" 时，才会启用 HTTP/2 支持。
//
// Serve 始终返回一个非 nil 的 error。
func Serve(l net.Listener, handler Handler) error {
	srv := &Server{Handler: handler} // 创建一个新的 Server 实例，并将 handler 赋值给它
	return srv.Serve(l)              // 调用 Server 实例的 Serve 方法开始监听和处理传入连接
}

// ServeTLS accepts incoming HTTPS connections on the listener l,
// creating a new service goroutine for each. The service goroutines
// read requests and then call handler to reply to them.
//
// The handler is typically nil, in which case [DefaultServeMux] is used.
//
// Additionally, files containing a certificate and matching private key
// for the server must be provided. If the certificate is signed by a
// certificate authority, the certFile should be the concatenation
// of the server's certificate, any intermediates, and the CA's certificate.
//
// ServeTLS always returns a non-nil error.
// ServeTLS 接受监听器 l 上的传入 HTTPS 连接，为每个连接创建一个新的服务 goroutine。
// 这些服务 goroutine 读取请求，然后调用 handler 来回复它们。
//
// handler 通常为 nil，在这种情况下，将使用 DefaultServeMux。
//
// 此外，必须提供包含服务器的证书和匹配的私钥的文件。如果证书由证书颁发机构签名，
// 则 certFile 应该是服务器的证书、任何中间证书和 CA 的证书的串联。
//
// ServeTLS 始终返回一个非 nil 的 error。
func ServeTLS(l net.Listener, handler Handler, certFile, keyFile string) error {
	srv := &Server{Handler: handler}          // 创建一个新的 Server 实例，并将 handler 赋值给它
	return srv.ServeTLS(l, certFile, keyFile) // 调用 Server 实例的 ServeTLS 方法开始监听和处理传入的 HTTPS 连接
}

// A Server defines parameters for running an HTTP server.
// The zero value for Server is a valid configuration.
// Server 结构体定义了运行 HTTP 服务器的参数。
// Server 的零值是一个有效的配置。
type Server struct {
	// Addr optionally specifies the TCP address for the server to listen on,
	// in the form "host:port". If empty, ":http" (port 80) is used.
	// The service names are defined in RFC 6335 and assigned by IANA.
	// See net.Dial for details of the address format.
	// Addr 可选地指定服务器监听的 TCP 地址，
	// 格式为 "host:port"。如果为空，则使用 ":http" (端口 80)。
	// 服务名称在 RFC 6335 中定义，并由 IANA 分配。
	// 有关地址格式的详细信息，请参见 net.Dial。
	Addr string

	Handler Handler // handler to invoke, http.DefaultServeMux if nil
	// Handler 是要调用的处理器，如果为 nil，则使用 http.DefaultServeMux。

	// DisableGeneralOptionsHandler, if true, passes "OPTIONS *" requests to the Handler,
	// otherwise responds with 200 OK and Content-Length: 0.
	// DisableGeneralOptionsHandler，如果为 true，则将 "OPTIONS *" 请求传递给 Handler，
	// 否则以 200 OK 和 Content-Length: 0 响应。
	DisableGeneralOptionsHandler bool

	// TLSConfig optionally provides a TLS configuration for use
	// by ServeTLS and ListenAndServeTLS. Note that this value is
	// cloned by ServeTLS and ListenAndServeTLS, so it's not
	// possible to modify the configuration with methods like
	// tls.Config.SetSessionTicketKeys. To use
	// SetSessionTicketKeys, use Server.Serve with a TLS Listener
	// instead.
	// TLSConfig 可选地提供一个 TLS 配置，供 ServeTLS 和 ListenAndServeTLS 使用。
	// 请注意，此值由 ServeTLS 和 ListenAndServeTLS 克隆，因此无法使用诸如
	// tls.Config.SetSessionTicketKeys 之类的方法修改配置。要使用
	// SetSessionTicketKeys，请改用带有 TLS Listener 的 Server.Serve。
	TLSConfig *tls.Config

	// ReadTimeout is the maximum duration for reading the entire
	// request, including the body. A zero or negative value means
	// there will be no timeout.
	//
	// Because ReadTimeout does not let Handlers make per-request
	// decisions on each request body's acceptable deadline or
	// upload rate, most users will prefer to use
	// ReadHeaderTimeout. It is valid to use them both.
	// ReadTimeout 是读取整个请求（包括请求体）的最大持续时间。零或负值表示没有超时。
	// 因为 ReadTimeout 不允许 Handlers 对每个请求体的可接受截止时间或上传速率做出每个请求的决策，
	// 所以大多数用户会更喜欢使用 ReadHeaderTimeout。同时使用两者是有效的。
	ReadTimeout time.Duration

	// ReadHeaderTimeout is the amount of time allowed to read
	// request headers. The connection's read deadline is reset
	// after reading the headers and the Handler can decide what
	// is considered too slow for the body. If zero, the value of
	// ReadTimeout is used. If negative, or if zero and ReadTimeout
	// is zero or negative, there is no timeout.
	// ReadHeaderTimeout 是允许读取请求头的最大时间。读取头后，连接的读取截止时间会重置，
	// 并且 Handler 可以决定对于请求体来说什么速度太慢。如果为零，则使用 ReadTimeout 的值。
	// 如果为负数，或者如果为零且 ReadTimeout 为零或负数，则没有超时。
	ReadHeaderTimeout time.Duration

	// WriteTimeout is the maximum duration before timing out
	// writes of the response. It is reset whenever a new
	// request's header is read. Like ReadTimeout, it does not
	// let Handlers make decisions on a per-request basis.
	// A zero or negative value means there will be no timeout.
	// WriteTimeout 是响应写入超时之前的最大持续时间。每当读取新请求的头时，它都会重置。
	// 像 ReadTimeout 一样，它不允许 Handlers 基于每个请求做出决策。
	// 零或负值表示没有超时。
	WriteTimeout time.Duration

	// IdleTimeout is the maximum amount of time to wait for the
	// next request when keep-alives are enabled. If zero, the value
	// of ReadTimeout is used. If negative, or if zero and ReadTimeout
	// is zero or negative, there is no timeout.
	// IdleTimeout 是在启用 keep-alive 时等待下一个请求的最大时间。如果为零，则使用 ReadTimeout 的值。
	// 如果为负数，或者如果为零且 ReadTimeout 为零或负数，则没有超时。
	IdleTimeout time.Duration

	// MaxHeaderBytes controls the maximum number of bytes the
	// server will read parsing the request header's keys and
	// values, including the request line. It does not limit the
	// size of the request body.
	// If zero, DefaultMaxHeaderBytes is used.
	// MaxHeaderBytes 控制服务器将读取的请求头键和值的最大字节数，包括请求行。它不限制请求体的大小。
	// 如果为零，则使用 DefaultMaxHeaderBytes。
	MaxHeaderBytes int

	// TLSNextProto optionally specifies a function to take over
	// ownership of the provided TLS connection when an ALPN
	// protocol upgrade has occurred. The map key is the protocol
	// name negotiated. The Handler argument should be used to
	// handle HTTP requests and will initialize the Request's TLS
	// and RemoteAddr if not already set. The connection is
	// automatically closed when the function returns.
	// If TLSNextProto is not nil, HTTP/2 support is not enabled
	// automatically.
	// TLSNextProto 可选地指定一个函数，用于在发生 ALPN 协议升级时接管提供的 TLS 连接的所有权。
	// map 键是协商的协议名称。Handler 参数应用于处理 HTTP 请求，如果尚未设置，将初始化 Request 的 TLS 和 RemoteAddr。
	// 函数返回时，连接将自动关闭。如果 TLSNextProto 不为 nil，则不会自动启用 HTTP/2 支持。
	TLSNextProto map[string]func(*Server, *tls.Conn, Handler)

	// ConnState specifies an optional callback function that is
	// called when a client connection changes state. See the
	// ConnState type and associated constants for details.
	// ConnState 指定一个可选的回调函数，该函数在客户端连接状态更改时被调用。
	// 有关详细信息，请参见 ConnState 类型和相关的常量。
	ConnState func(net.Conn, ConnState)

	// ErrorLog specifies an optional logger for errors accepting
	// connections, unexpected behavior from handlers, and
	// underlying FileSystem errors.
	// If nil, logging is done via the log package's standard logger.
	// ErrorLog 指定一个可选的记录器，用于记录接受连接时的错误、处理程序的意外行为以及底层文件系统错误。
	// 如果为 nil，则通过 log 包的标准记录器完成日志记录。
	ErrorLog *log.Logger

	// BaseContext optionally specifies a function that returns
	// the base context for incoming requests on this server.
	// The provided Listener is the specific Listener that's
	// about to start accepting requests.
	// If BaseContext is nil, the default is context.Background().
	// If non-nil, it must return a non-nil context.
	// BaseContext 可选地指定一个函数，该函数返回此服务器上接收请求的基本上下文。
	// 提供的 Listener 是即将开始接受请求的特定 Listener。
	// 如果 BaseContext 为 nil，则默认值为 context.Background()。
	// 如果非 nil，则必须返回一个非 nil 的上下文。
	BaseContext func(net.Listener) context.Context

	// ConnContext optionally specifies a function that modifies
	// the context used for a new connection c. The provided ctx
	// is derived from the base context and has a ServerContextKey
	// value.
	// ConnContext 可选地指定一个函数，该函数修改用于新连接 c 的上下文。提供的 ctx 派生自基本上下文，并具有 ServerContextKey 值。
	ConnContext func(ctx context.Context, c net.Conn) context.Context

	inShutdown atomic.Bool // true when server is in shutdown  // inShutdown 是一个原子布尔值，当服务器正在关闭时为 true

	disableKeepAlives atomic.Bool // disableKeepAlives 是一个原子布尔值，用于禁用 keep-alive 连接
	nextProtoOnce     sync.Once   // guards setupHTTP2_* init // nextProtoOnce 用于保护 setupHTTP2_* 的初始化，确保只执行一次
	nextProtoErr      error       // result of http2.ConfigureServer if used // nextProtoErr 是 http2.ConfigureServer 的结果，如果使用了 HTTP/2 配置

	mu         sync.Mutex                 // mu 是一个互斥锁，用于保护 listeners, activeConn, onShutdown 等字段
	listeners  map[*net.Listener]struct{} // listeners 存储服务器正在监听的 net.Listener 集合
	activeConn map[*conn]struct{}         // activeConn 存储服务器当前活跃的连接集合
	onShutdown []func()                   // onShutdown 存储服务器关闭时需要执行的函数列表

	listenerGroup sync.WaitGroup // listenerGroup 用于等待所有 listener 关闭
}

// Close immediately closes all active net.Listeners and any
// connections in state [StateNew], [StateActive], or [StateIdle]. For a
// graceful shutdown, use [Server.Shutdown].
//
// Close does not attempt to close (and does not even know about)
// any hijacked connections, such as WebSockets.
//
// Close returns any error returned from closing the [Server]'s
// underlying Listener(s).
// Close 方法立即关闭所有活动的 net.Listeners 以及处于 [StateNew]、[StateActive] 或 [StateIdle] 状态的任何连接。
// 要实现优雅关闭，请使用 [Server.Shutdown]。
//
// Close 方法不会尝试关闭（甚至不知道）任何被劫持的连接，例如 WebSockets。
//
// Close 方法返回关闭 [Server] 的底层 Listener 时返回的任何错误。
func (srv *Server) Close() error {
	srv.inShutdown.Store(true) // 设置 inShutdown 标志为 true，表示服务器正在关闭 Set the inShutdown flag to true, indicating that the server is closing
	srv.mu.Lock()              // 获取互斥锁，保护 listeners 和 activeConn 字段 Acquire mutex to protect listeners and activeConn fields
	defer srv.mu.Unlock()      // 延迟释放互斥锁 Defer releasing the mutex

	err := srv.closeListenersLocked() // 关闭所有 listener Close all listeners

	// Unlock srv.mu while waiting for listenerGroup.
	// The group Add and Done calls are made with srv.mu held,
	// to avoid adding a new listener in the window between
	// us setting inShutdown above and waiting here.
	// 在等待 listenerGroup 时释放 srv.mu。
	// group 的 Add 和 Done 调用是在持有 srv.mu 的情况下进行的，
	// 以避免在我们设置上面的 inShutdown 和在此处等待之间的窗口中添加新的 listener。
	srv.mu.Unlock()          // 释放互斥锁 Unlock the mutex
	srv.listenerGroup.Wait() // 等待所有 listener 关闭 Wait for all listeners to close
	srv.mu.Lock()            // 重新获取互斥锁 Reacquire the mutex

	for c := range srv.activeConn { // 遍历所有活跃的连接 Iterate over all active connections
		c.rwc.Close()             // 关闭连接 Close the connection
		delete(srv.activeConn, c) // 从活跃连接集合中删除连接 Remove the connection from the active connection set
	}
	return err // 返回关闭 listener 时返回的错误 Return the error returned when closing the listener
}

// shutdownPollIntervalMax is the max polling interval when checking
// quiescence during Server.Shutdown. Polling starts with a small
// interval and backs off to the max.
// Ideally we could find a solution that doesn't involve polling,
// but which also doesn't have a high runtime cost (and doesn't
// involve any contentious mutexes), but that is left as an
// exercise for the reader.
const shutdownPollIntervalMax = 500 * time.Millisecond

// Shutdown gracefully shuts down the server without interrupting any
// active connections. Shutdown works by first closing all open
// listeners, then closing all idle connections, and then waiting
// indefinitely for connections to return to idle and then shut down.
// If the provided context expires before the shutdown is complete,
// Shutdown returns the context's error, otherwise it returns any
// error returned from closing the [Server]'s underlying Listener(s).
//
// When Shutdown is called, [Serve], [ListenAndServe], and
// [ListenAndServeTLS] immediately return [ErrServerClosed]. Make sure the
// program doesn't exit and waits instead for Shutdown to return.
//
// Shutdown does not attempt to close nor wait for hijacked
// connections such as WebSockets. The caller of Shutdown should
// separately notify such long-lived connections of shutdown and wait
// for them to close, if desired. See [Server.RegisterOnShutdown] for a way to
// register shutdown notification functions.
//
// Once Shutdown has been called on a server, it may not be reused;
// future calls to methods such as Serve will return ErrServerClosed.
func (srv *Server) Shutdown(ctx context.Context) error {
	srv.inShutdown.Store(true)

	srv.mu.Lock()
	lnerr := srv.closeListenersLocked()
	for _, f := range srv.onShutdown {
		go f()
	}
	srv.mu.Unlock()
	srv.listenerGroup.Wait()

	pollIntervalBase := time.Millisecond
	nextPollInterval := func() time.Duration {
		// Add 10% jitter.
		interval := pollIntervalBase + time.Duration(rand.Intn(int(pollIntervalBase/10)))
		// Double and clamp for next time.
		pollIntervalBase *= 2
		if pollIntervalBase > shutdownPollIntervalMax {
			pollIntervalBase = shutdownPollIntervalMax
		}
		return interval
	}

	timer := time.NewTimer(nextPollInterval())
	defer timer.Stop()
	for {
		if srv.closeIdleConns() {
			return lnerr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			timer.Reset(nextPollInterval())
		}
	}
}

// RegisterOnShutdown registers a function to call on [Server.Shutdown].
// This can be used to gracefully shutdown connections that have
// undergone ALPN protocol upgrade or that have been hijacked.
// This function should start protocol-specific graceful shutdown,
// but should not wait for shutdown to complete.
func (srv *Server) RegisterOnShutdown(f func()) {
	srv.mu.Lock()
	srv.onShutdown = append(srv.onShutdown, f)
	srv.mu.Unlock()
}

// closeIdleConns closes all idle connections and reports whether the
// server is quiescent.
func (s *Server) closeIdleConns() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	quiescent := true
	for c := range s.activeConn {
		st, unixSec := c.getState()
		// Issue 22682: treat StateNew connections as if
		// they're idle if we haven't read the first request's
		// header in over 5 seconds.
		if st == StateNew && unixSec < time.Now().Unix()-5 {
			st = StateIdle
		}
		if st != StateIdle || unixSec == 0 {
			// Assume unixSec == 0 means it's a very new
			// connection, without state set yet.
			quiescent = false
			continue
		}
		c.rwc.Close()
		delete(s.activeConn, c)
	}
	return quiescent
}

func (s *Server) closeListenersLocked() error {
	var err error
	for ln := range s.listeners {
		if cerr := (*ln).Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// A ConnState represents the state of a client connection to a server.
// It's used by the optional [Server.ConnState] hook.
type ConnState int

const (
	// StateNew represents a new connection that is expected to
	// send a request immediately. Connections begin at this
	// state and then transition to either StateActive or
	// StateClosed.
	StateNew ConnState = iota

	// StateActive represents a connection that has read 1 or more
	// bytes of a request. The Server.ConnState hook for
	// StateActive fires before the request has entered a handler
	// and doesn't fire again until the request has been
	// handled. After the request is handled, the state
	// transitions to StateClosed, StateHijacked, or StateIdle.
	// For HTTP/2, StateActive fires on the transition from zero
	// to one active request, and only transitions away once all
	// active requests are complete. That means that ConnState
	// cannot be used to do per-request work; ConnState only notes
	// the overall state of the connection.
	StateActive

	// StateIdle represents a connection that has finished
	// handling a request and is in the keep-alive state, waiting
	// for a new request. Connections transition from StateIdle
	// to either StateActive or StateClosed.
	StateIdle

	// StateHijacked represents a hijacked connection.
	// This is a terminal state. It does not transition to StateClosed.
	StateHijacked

	// StateClosed represents a closed connection.
	// This is a terminal state. Hijacked connections do not
	// transition to StateClosed.
	StateClosed
)

var stateName = map[ConnState]string{
	StateNew:      "new",
	StateActive:   "active",
	StateIdle:     "idle",
	StateHijacked: "hijacked",
	StateClosed:   "closed",
}

func (c ConnState) String() string {
	return stateName[c]
}

// serverHandler delegates to either the server's Handler or
// DefaultServeMux and also handles "OPTIONS *" requests.
type serverHandler struct {
	srv *Server
}

// ServeHTTP should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/erda-project/erda-infra
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname badServeHTTP net/http.serverHandler.ServeHTTP
func (sh serverHandler) ServeHTTP(rw ResponseWriter, req *Request) {
	handler := sh.srv.Handler
	if handler == nil {
		handler = DefaultServeMux
	}
	if !sh.srv.DisableGeneralOptionsHandler && req.RequestURI == "*" && req.Method == "OPTIONS" {
		handler = globalOptionsHandler{}
	}

	handler.ServeHTTP(rw, req)
}

func badServeHTTP(serverHandler, ResponseWriter, *Request)

// AllowQuerySemicolons returns a handler that serves requests by converting any
// unescaped semicolons in the URL query to ampersands, and invoking the handler h.
//
// This restores the pre-Go 1.17 behavior of splitting query parameters on both
// semicolons and ampersands. (See golang.org/issue/25192). Note that this
// behavior doesn't match that of many proxies, and the mismatch can lead to
// security issues.
//
// AllowQuerySemicolons should be invoked before [Request.ParseForm] is called.
func AllowQuerySemicolons(h Handler) Handler {
	return HandlerFunc(func(w ResponseWriter, r *Request) {
		if strings.Contains(r.URL.RawQuery, ";") {
			r2 := new(Request)
			*r2 = *r
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.RawQuery = strings.ReplaceAll(r.URL.RawQuery, ";", "&")
			h.ServeHTTP(w, r2)
		} else {
			h.ServeHTTP(w, r)
		}
	})
}

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls [Serve] to handle requests on incoming connections.
// Accepted connections are configured to enable TCP keep-alives.
//
// If srv.Addr is blank, ":http" is used.
//
// ListenAndServe always returns a non-nil error. After [Server.Shutdown] or [Server.Close],
// the returned error is [ErrServerClosed].
// ListenAndServe 监听 TCP 网络地址 srv.Addr，然后调用 Serve 方法来处理传入连接上的请求。
// 接受的连接被配置为启用 TCP keep-alive。
//
// 如果 srv.Addr 为空，则使用 ":http"。
//
// ListenAndServe 始终返回一个非 nil 的 error。在调用 Server.Shutdown 或 Server.Close 之后，
// 返回的 error 是 ErrServerClosed。
func (srv *Server) ListenAndServe() error {
	if srv.shuttingDown() { // 检查服务器是否正在关闭
		return ErrServerClosed // 如果服务器正在关闭，则返回 ErrServerClosed 错误
	}
	addr := srv.Addr // 获取服务器地址
	if addr == "" {  // 如果地址为空
		addr = ":http" // 设置默认地址为 ":http"
	}
	ln, err := net.Listen("tcp", addr) // 在 TCP 地址上监听
	if err != nil {                    // 如果监听失败
		return err // 返回错误
	}
	return srv.Serve(ln) // 使用 listener 提供服务
}

var testHookServerServe func(*Server, net.Listener) // used if non-nil

// shouldConfigureHTTP2ForServe reports whether Server.Serve should configure
// automatic HTTP/2. (which sets up the srv.TLSNextProto map)
func (srv *Server) shouldConfigureHTTP2ForServe() bool {
	if srv.TLSConfig == nil {
		// Compatibility with Go 1.6:
		// If there's no TLSConfig, it's possible that the user just
		// didn't set it on the http.Server, but did pass it to
		// tls.NewListener and passed that listener to Serve.
		// So we should configure HTTP/2 (to set up srv.TLSNextProto)
		// in case the listener returns an "h2" *tls.Conn.
		return true
	}
	// The user specified a TLSConfig on their http.Server.
	// In this, case, only configure HTTP/2 if their tls.Config
	// explicitly mentions "h2". Otherwise http2.ConfigureServer
	// would modify the tls.Config to add it, but they probably already
	// passed this tls.Config to tls.NewListener. And if they did,
	// it's too late anyway to fix it. It would only be potentially racy.
	// See Issue 15908.
	return slices.Contains(srv.TLSConfig.NextProtos, http2NextProtoTLS)
}

// ErrServerClosed is returned by the [Server.Serve], [ServeTLS], [ListenAndServe],
// and [ListenAndServeTLS] methods after a call to [Server.Shutdown] or [Server.Close].
var ErrServerClosed = errors.New("http: Server closed")

// Serve accepts incoming connections on the Listener l, creating a
// new service goroutine for each. The service goroutines read requests and
// then call srv.Handler to reply to them.
//
// HTTP/2 support is only enabled if the Listener returns [*tls.Conn]
// connections and they were configured with "h2" in the TLS
// Config.NextProtos.
//
// Serve always returns a non-nil error and closes l.
// After [Server.Shutdown] or [Server.Close], the returned error is [ErrServerClosed].
func (srv *Server) Serve(l net.Listener) error {
	if fn := testHookServerServe; fn != nil {
		fn(srv, l) // call hook with unwrapped listener
	}

	origListener := l
	l = &onceCloseListener{Listener: l}
	defer l.Close()

	if err := srv.setupHTTP2_Serve(); err != nil {
		return err
	}

	if !srv.trackListener(&l, true) {
		return ErrServerClosed
	}
	defer srv.trackListener(&l, false)

	baseCtx := context.Background()
	if srv.BaseContext != nil {
		baseCtx = srv.BaseContext(origListener)
		if baseCtx == nil {
			panic("BaseContext returned a nil context")
		}
	}

	var tempDelay time.Duration // how long to sleep on accept failure

	ctx := context.WithValue(baseCtx, ServerContextKey, srv)
	for {
		rw, err := l.Accept()
		if err != nil {
			if srv.shuttingDown() {
				return ErrServerClosed
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				srv.logf("http: Accept error: %v; retrying in %v", err, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return err
		}
		connCtx := ctx
		if cc := srv.ConnContext; cc != nil {
			connCtx = cc(connCtx, rw)
			if connCtx == nil {
				panic("ConnContext returned nil")
			}
		}
		tempDelay = 0
		c := srv.newConn(rw)
		c.setState(c.rwc, StateNew, runHooks) // before Serve can return
		go c.serve(connCtx)
	}
}

// ServeTLS accepts incoming connections on the Listener l, creating a
// new service goroutine for each. The service goroutines perform TLS
// setup and then read requests, calling srv.Handler to reply to them.
//
// Files containing a certificate and matching private key for the
// server must be provided if neither the [Server]'s
// TLSConfig.Certificates, TLSConfig.GetCertificate nor
// config.GetConfigForClient are populated.
// If the certificate is signed by a certificate authority, the
// certFile should be the concatenation of the server's certificate,
// any intermediates, and the CA's certificate.
//
// ServeTLS always returns a non-nil error. After [Server.Shutdown] or [Server.Close], the
// returned error is [ErrServerClosed].
func (srv *Server) ServeTLS(l net.Listener, certFile, keyFile string) error {
	// Setup HTTP/2 before srv.Serve, to initialize srv.TLSConfig
	// before we clone it and create the TLS Listener.
	if err := srv.setupHTTP2_ServeTLS(); err != nil {
		return err
	}

	config := cloneTLSConfig(srv.TLSConfig)
	if !slices.Contains(config.NextProtos, "http/1.1") {
		config.NextProtos = append(config.NextProtos, "http/1.1")
	}

	configHasCert := len(config.Certificates) > 0 || config.GetCertificate != nil || config.GetConfigForClient != nil
	if !configHasCert || certFile != "" || keyFile != "" {
		var err error
		config.Certificates = make([]tls.Certificate, 1)
		config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return err
		}
	}

	tlsListener := tls.NewListener(l, config)
	return srv.Serve(tlsListener)
}

// trackListener adds or removes a net.Listener to the set of tracked
// listeners.
//
// We store a pointer to interface in the map set, in case the
// net.Listener is not comparable. This is safe because we only call
// trackListener via Serve and can track+defer untrack the same
// pointer to local variable there. We never need to compare a
// Listener from another caller.
//
// It reports whether the server is still up (not Shutdown or Closed).
func (s *Server) trackListener(ln *net.Listener, add bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listeners == nil {
		s.listeners = make(map[*net.Listener]struct{})
	}
	if add {
		if s.shuttingDown() {
			return false
		}
		s.listeners[ln] = struct{}{}
		s.listenerGroup.Add(1)
	} else {
		delete(s.listeners, ln)
		s.listenerGroup.Done()
	}
	return true
}

func (s *Server) trackConn(c *conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeConn == nil {
		s.activeConn = make(map[*conn]struct{})
	}
	if add {
		s.activeConn[c] = struct{}{}
	} else {
		delete(s.activeConn, c)
	}
}

func (s *Server) idleTimeout() time.Duration {
	if s.IdleTimeout != 0 {
		return s.IdleTimeout
	}
	return s.ReadTimeout
}

func (s *Server) readHeaderTimeout() time.Duration {
	if s.ReadHeaderTimeout != 0 {
		return s.ReadHeaderTimeout
	}
	return s.ReadTimeout
}

func (s *Server) doKeepAlives() bool {
	return !s.disableKeepAlives.Load() && !s.shuttingDown()
}

func (s *Server) shuttingDown() bool {
	return s.inShutdown.Load()
}

// SetKeepAlivesEnabled controls whether HTTP keep-alives are enabled.
// By default, keep-alives are always enabled. Only very
// resource-constrained environments or servers in the process of
// shutting down should disable them.
func (srv *Server) SetKeepAlivesEnabled(v bool) {
	if v {
		srv.disableKeepAlives.Store(false)
		return
	}
	srv.disableKeepAlives.Store(true)

	// Close idle HTTP/1 conns:
	srv.closeIdleConns()

	// TODO: Issue 26303: close HTTP/2 conns as soon as they become idle.
}

func (s *Server) logf(format string, args ...any) {
	if s.ErrorLog != nil {
		s.ErrorLog.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// logf prints to the ErrorLog of the *Server associated with request r
// via ServerContextKey. If there's no associated server, or if ErrorLog
// is nil, logging is done via the log package's standard logger.
func logf(r *Request, format string, args ...any) {
	s, _ := r.Context().Value(ServerContextKey).(*Server)
	if s != nil && s.ErrorLog != nil {
		s.ErrorLog.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// ListenAndServe listens on the TCP network address addr and then calls
// [Serve] with handler to handle requests on incoming connections.
// Accepted connections are configured to enable TCP keep-alives.
//
// The handler is typically nil, in which case [DefaultServeMux] is used.
//
// ListenAndServe always returns a non-nil error.
//
// ListenAndServe 监听 TCP 网络地址 addr，然后调用 Serve 方法，使用 handler 处理传入连接上的请求。
// 接受的连接被配置为启用 TCP keep-alive。
//
// handler 通常为 nil，在这种情况下，将使用 DefaultServeMux。
//
// ListenAndServe 始终返回一个非 nil 的 error。
func ListenAndServe(addr string, handler Handler) error {
	server := &Server{Addr: addr, Handler: handler} // 创建一个新的 Server 实例，设置地址和处理器
	return server.ListenAndServe()                  // 调用 Server 实例的 ListenAndServe 方法开始监听和处理请求
}

// ListenAndServeTLS acts identically to [ListenAndServe], except that it
// expects HTTPS connections. Additionally, files containing a certificate and
// matching private key for the server must be provided. If the certificate
// is signed by a certificate authority, the certFile should be the concatenation
// of the server's certificate, any intermediates, and the CA's certificate.
// ListenAndServeTLS 的行为与 ListenAndServe 相同，除了它期望 HTTPS 连接。此外，必须提供包含服务器证书和匹配私钥的文件。
// 如果证书由证书颁发机构签名，则 certFile 应该是服务器证书、任何中间证书和 CA 证书的串联。
func ListenAndServeTLS(addr, certFile, keyFile string, handler Handler) error {
	server := &Server{Addr: addr, Handler: handler}    // 创建一个新的 Server 实例，设置地址和处理器
	return server.ListenAndServeTLS(certFile, keyFile) // 调用 Server 实例的 ListenAndServeTLS 方法开始监听和处理 TLS 请求
}

// ListenAndServeTLS listens on the TCP network address srv.Addr and
// then calls [ServeTLS] to handle requests on incoming TLS connections.
// Accepted connections are configured to enable TCP keep-alives.
//
// Filenames containing a certificate and matching private key for the
// server must be provided if neither the [Server]'s TLSConfig.Certificates
// nor TLSConfig.GetCertificate are populated. If the certificate is
// signed by a certificate authority, the certFile should be the
// concatenation of the server's certificate, any intermediates, and
// the CA's certificate.
//
// If srv.Addr is blank, ":https" is used.
//
// ListenAndServeTLS always returns a non-nil error. After [Server.Shutdown] or
// [Server.Close], the returned error is [ErrServerClosed].
// ListenAndServeTLS 监听 TCP 网络地址 srv.Addr，然后调用 ServeTLS 来处理传入的 TLS 连接请求。
// 接受的连接被配置为启用 TCP keep-alive。
//
// 如果既没有配置 [Server] 的 TLSConfig.Certificates，也没有配置 TLSConfig.GetCertificate，
// 则必须提供包含服务器证书和匹配私钥的文件名。如果证书由证书颁发机构签名，
// 则 certFile 应该是服务器证书、任何中间证书和 CA 证书的串联。
//
// 如果 srv.Addr 为空，则使用 ":https"。
//
// ListenAndServeTLS 始终返回一个非 nil 的 error。在 [Server.Shutdown] 或 [Server.Close] 之后，
// 返回的 error 是 [ErrServerClosed]。
func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {
	if srv.shuttingDown() { // 检查服务器是否正在关闭
		return ErrServerClosed // 如果服务器正在关闭，则返回 ErrServerClosed 错误
	}
	addr := srv.Addr // 获取服务器地址
	if addr == "" {  // 如果地址为空
		addr = ":https" // 设置默认地址为 ":https"
	}

	ln, err := net.Listen("tcp", addr) // 在 TCP 地址上监听
	if err != nil {                    // 如果监听失败
		return err // 返回错误
	}

	defer ln.Close() // 延迟关闭 listener

	return srv.ServeTLS(ln, certFile, keyFile) // 使用 TLS 配置服务 listener
}

// setupHTTP2_ServeTLS conditionally configures HTTP/2 on
// srv and reports whether there was an error setting it up. If it is
// not configured for policy reasons, nil is returned.
// setupHTTP2_ServeTLS 有条件地在 srv 上配置 HTTP/2，并报告设置过程中是否发生错误。如果由于策略原因未配置，则返回 nil。
func (srv *Server) setupHTTP2_ServeTLS() error {
	srv.nextProtoOnce.Do(srv.onceSetNextProtoDefaults) // 确保只设置一次 NextProto 的默认值。Ensure that NextProto defaults are set only once.
	return srv.nextProtoErr                            // 返回设置 NextProto 过程中可能发生的错误。Return any errors that may have occurred while setting NextProto.
}

// setupHTTP2_Serve is called from (*Server).Serve and conditionally
// configures HTTP/2 on srv using a more conservative policy than
// setupHTTP2_ServeTLS because Serve is called after tls.Listen,
// and may be called concurrently. See shouldConfigureHTTP2ForServe.
//
// The tests named TestTransportAutomaticHTTP2* and
// TestConcurrentServerServe in server_test.go demonstrate some
// of the supported use cases and motivations.
func (srv *Server) setupHTTP2_Serve() error {
	srv.nextProtoOnce.Do(srv.onceSetNextProtoDefaults_Serve)
	return srv.nextProtoErr
}

func (srv *Server) onceSetNextProtoDefaults_Serve() {
	if srv.shouldConfigureHTTP2ForServe() {
		srv.onceSetNextProtoDefaults()
	}
}

var http2server = godebug.New("http2server")

// onceSetNextProtoDefaults configures HTTP/2, if the user hasn't
// configured otherwise. (by setting srv.TLSNextProto non-nil)
// It must only be called via srv.nextProtoOnce (use srv.setupHTTP2_*).
func (srv *Server) onceSetNextProtoDefaults() {
	if omitBundledHTTP2 {
		return
	}
	if http2server.Value() == "0" {
		http2server.IncNonDefault()
		return
	}
	// Enable HTTP/2 by default if the user hasn't otherwise
	// configured their TLSNextProto map.
	if srv.TLSNextProto == nil {
		conf := &http2Server{}
		srv.nextProtoErr = http2ConfigureServer(srv, conf)
	}
}

// TimeoutHandler returns a [Handler] that runs h with the given time limit.
//
// The new Handler calls h.ServeHTTP to handle each request, but if a
// call runs for longer than its time limit, the handler responds with
// a 503 Service Unavailable error and the given message in its body.
// (If msg is empty, a suitable default message will be sent.)
// After such a timeout, writes by h to its [ResponseWriter] will return
// [ErrHandlerTimeout].
//
// TimeoutHandler supports the [Pusher] interface but does not support
// the [Hijacker] or [Flusher] interfaces.
func TimeoutHandler(h Handler, dt time.Duration, msg string) Handler {
	return &timeoutHandler{
		handler: h,
		body:    msg,
		dt:      dt,
	}
}

// ErrHandlerTimeout is returned on [ResponseWriter] Write calls
// in handlers which have timed out.
var ErrHandlerTimeout = errors.New("http: Handler timeout")

type timeoutHandler struct {
	handler Handler
	body    string
	dt      time.Duration

	// When set, no context will be created and this context will
	// be used instead.
	testContext context.Context
}

func (h *timeoutHandler) errorBody() string {
	if h.body != "" {
		return h.body
	}
	return "<html><head><title>Timeout</title></head><body><h1>Timeout</h1></body></html>"
}

func (h *timeoutHandler) ServeHTTP(w ResponseWriter, r *Request) {
	ctx := h.testContext
	if ctx == nil {
		var cancelCtx context.CancelFunc
		ctx, cancelCtx = context.WithTimeout(r.Context(), h.dt)
		defer cancelCtx()
	}
	r = r.WithContext(ctx)
	done := make(chan struct{})
	tw := &timeoutWriter{
		w:   w,
		h:   make(Header),
		req: r,
	}
	panicChan := make(chan any, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				panicChan <- p
			}
		}()
		h.handler.ServeHTTP(tw, r)
		close(done)
	}()
	select {
	case p := <-panicChan:
		panic(p)
	case <-done:
		tw.mu.Lock()
		defer tw.mu.Unlock()
		dst := w.Header()
		for k, vv := range tw.h {
			dst[k] = vv
		}
		if !tw.wroteHeader {
			tw.code = StatusOK
		}
		w.WriteHeader(tw.code)
		w.Write(tw.wbuf.Bytes())
	case <-ctx.Done():
		tw.mu.Lock()
		defer tw.mu.Unlock()
		switch err := ctx.Err(); err {
		case context.DeadlineExceeded:
			w.WriteHeader(StatusServiceUnavailable)
			io.WriteString(w, h.errorBody())
			tw.err = ErrHandlerTimeout
		default:
			w.WriteHeader(StatusServiceUnavailable)
			tw.err = err
		}
	}
}

type timeoutWriter struct {
	w    ResponseWriter
	h    Header
	wbuf bytes.Buffer
	req  *Request

	mu          sync.Mutex
	err         error
	wroteHeader bool
	code        int
}

var _ Pusher = (*timeoutWriter)(nil)

// Push implements the [Pusher] interface.
func (tw *timeoutWriter) Push(target string, opts *PushOptions) error {
	if pusher, ok := tw.w.(Pusher); ok {
		return pusher.Push(target, opts)
	}
	return ErrNotSupported
}

func (tw *timeoutWriter) Header() Header { return tw.h }

func (tw *timeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.err != nil {
		return 0, tw.err
	}
	if !tw.wroteHeader {
		tw.writeHeaderLocked(StatusOK)
	}
	return tw.wbuf.Write(p)
}

func (tw *timeoutWriter) writeHeaderLocked(code int) {
	checkWriteHeaderCode(code)

	switch {
	case tw.err != nil:
		return
	case tw.wroteHeader:
		if tw.req != nil {
			caller := relevantCaller()
			logf(tw.req, "http: superfluous response.WriteHeader call from %s (%s:%d)", caller.Function, path.Base(caller.File), caller.Line)
		}
	default:
		tw.wroteHeader = true
		tw.code = code
	}
}

func (tw *timeoutWriter) WriteHeader(code int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.writeHeaderLocked(code)
}

// onceCloseListener wraps a net.Listener, protecting it from
// multiple Close calls.
type onceCloseListener struct {
	net.Listener
	once     sync.Once
	closeErr error
}

func (oc *onceCloseListener) Close() error {
	oc.once.Do(oc.close)
	return oc.closeErr
}

func (oc *onceCloseListener) close() { oc.closeErr = oc.Listener.Close() }

// globalOptionsHandler responds to "OPTIONS *" requests.
type globalOptionsHandler struct{}

func (globalOptionsHandler) ServeHTTP(w ResponseWriter, r *Request) {
	w.Header().Set("Content-Length", "0")
	if r.ContentLength != 0 {
		// Read up to 4KB of OPTIONS body (as mentioned in the
		// spec as being reserved for future use), but anything
		// over that is considered a waste of server resources
		// (or an attack) and we abort and close the connection,
		// courtesy of MaxBytesReader's EOF behavior.
		mb := MaxBytesReader(w, r.Body, 4<<10)
		io.Copy(io.Discard, mb)
	}
}

// initALPNRequest is an HTTP handler that initializes certain
// uninitialized fields in its *Request. Such partially-initialized
// Requests come from ALPN protocol handlers.
type initALPNRequest struct {
	ctx context.Context
	c   *tls.Conn
	h   serverHandler
}

// BaseContext is an exported but unadvertised [http.Handler] method
// recognized by x/net/http2 to pass down a context; the TLSNextProto
// API predates context support so we shoehorn through the only
// interface we have available.
func (h initALPNRequest) BaseContext() context.Context { return h.ctx }

func (h initALPNRequest) ServeHTTP(rw ResponseWriter, req *Request) {
	if req.TLS == nil {
		req.TLS = &tls.ConnectionState{}
		*req.TLS = h.c.ConnectionState()
	}
	if req.Body == nil {
		req.Body = NoBody
	}
	if req.RemoteAddr == "" {
		req.RemoteAddr = h.c.RemoteAddr().String()
	}
	h.h.ServeHTTP(rw, req)
}

// loggingConn is used for debugging.
type loggingConn struct {
	name string
	net.Conn
}

var (
	uniqNameMu   sync.Mutex
	uniqNameNext = make(map[string]int)
)

func newLoggingConn(baseName string, c net.Conn) net.Conn {
	uniqNameMu.Lock()
	defer uniqNameMu.Unlock()
	uniqNameNext[baseName]++
	return &loggingConn{
		name: fmt.Sprintf("%s-%d", baseName, uniqNameNext[baseName]),
		Conn: c,
	}
}

func (c *loggingConn) Write(p []byte) (n int, err error) {
	log.Printf("%s.Write(%d) = ....", c.name, len(p))
	n, err = c.Conn.Write(p)
	log.Printf("%s.Write(%d) = %d, %v", c.name, len(p), n, err)
	return
}

func (c *loggingConn) Read(p []byte) (n int, err error) {
	log.Printf("%s.Read(%d) = ....", c.name, len(p))
	n, err = c.Conn.Read(p)
	log.Printf("%s.Read(%d) = %d, %v", c.name, len(p), n, err)
	return
}

func (c *loggingConn) Close() (err error) {
	log.Printf("%s.Close() = ...", c.name)
	err = c.Conn.Close()
	log.Printf("%s.Close() = %v", c.name, err)
	return
}

// checkConnErrorWriter writes to c.rwc and records any write errors to c.werr.
// It only contains one field (and a pointer field at that), so it
// fits in an interface value without an extra allocation.
type checkConnErrorWriter struct {
	c *conn
}

func (w checkConnErrorWriter) Write(p []byte) (n int, err error) {
	n, err = w.c.rwc.Write(p)
	if err != nil && w.c.werr == nil {
		w.c.werr = err
		w.c.cancelCtx()
	}
	return
}

func numLeadingCRorLF(v []byte) (n int) {
	for _, b := range v {
		if b == '\r' || b == '\n' {
			n++
			continue
		}
		break
	}
	return
}

// tlsRecordHeaderLooksLikeHTTP reports whether a TLS record header
// looks like it might've been a misdirected plaintext HTTP request.
func tlsRecordHeaderLooksLikeHTTP(hdr [5]byte) bool {
	switch string(hdr[:]) {
	case "GET /", "HEAD ", "POST ", "PUT /", "OPTIO":
		return true
	}
	return false
}

// MaxBytesHandler returns a [Handler] that runs h with its [ResponseWriter] and [Request.Body] wrapped by a MaxBytesReader.
func MaxBytesHandler(h Handler, n int64) Handler {
	return HandlerFunc(func(w ResponseWriter, r *Request) {
		r2 := *r
		r2.Body = MaxBytesReader(w, r.Body, n)
		h.ServeHTTP(w, &r2)
	})
}
