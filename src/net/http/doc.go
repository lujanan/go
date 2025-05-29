// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Package http provides HTTP client and server implementations.
// http 包提供了 HTTP 客户端和服务器的实现。

[Get], [Head], [Post], and [PostForm] make HTTP (or HTTPS) requests:
// Get、Head、Post 和 PostForm 函数用于发起 HTTP (或 HTTPS) 请求。
//
// 以下是一些使用这些函数进行 HTTP 请求的示例：
//
//	resp, err := http.Get("http://example.com/")
//	// 上述代码发送一个简单的 GET 请求到指定的 URL。
//	...
//	resp, err := http.Post("http://example.com/upload", "image/jpeg", &buf)
//	// 上述代码发送一个 POST 请求，其中包含指定的内容类型（例如 "image/jpeg"）和请求体（例如一个字节缓冲区）。
//	...
//	resp, err := http.PostForm("http://example.com/form",
//		url.Values{"key": {"Value"}, "id": {"123"}})
//	// 上述代码发送一个 POST 请求，其请求体是 URL 编码的表单数据（使用 url.Values 类型构建）。

The caller must close the response body when finished with it:
// 调用者在使用完响应体后必须关闭它，以确保资源被正确释放，避免资源泄露。

	resp, err := http.Get("http://example.com/")
	// 上述代码发送一个简单的 GET 请求到指定的 URL "http://example.com/"。
	if err != nil {
		// handle error
		// 如果在发起请求或接收响应时发生错误（例如网络问题、DNS解析失败等），应在此处进行错误处理。
	}
	defer resp.Body.Close()
	// 使用 defer 语句确保在当前函数执行完毕（无论是正常返回还是发生 panic）之前，
	// 都会调用 resp.Body.Close() 方法来关闭 HTTP 响应体。
	// 这是非常关键的步骤，因为不关闭响应体可能导致底层网络连接无法被复用或释放，
	// 从而耗尽文件描述符或网络资源，尤其是在高并发场景下。
	body, err := io.ReadAll(resp.Body)
	// io.ReadAll 函数从响应体 (resp.Body) 中读取所有数据，直到遇到 EOF (文件结束符) 或发生错误。
	// 它将读取到的数据作为字节切片返回。在读取完成后，通常会检查读取过程中是否发生错误。
	// ...
	// 在这里可以对读取到的响应体数据 (body) 进行进一步处理，例如解析 JSON、HTML 内容，或者将其写入文件。

# Clients and Transports
// 客户端和传输层：
// 这一部分将介绍如何通过创建自定义的 [Client] 和 [Transport] 类型来更精细地控制 HTTP 客户端的行为，
// 例如设置请求头、重定向策略、代理、TLS 配置、连接池管理、压缩等。

// For control over HTTP client headers, redirect policy, and other
// settings, create a [Client]:
// 为了更精细地控制 HTTP 客户端的请求头、重定向策略以及其他各种设置，可以创建一个 [Client] 实例：

	client := &http.Client{
		CheckRedirect: redirectPolicyFunc, // CheckRedirect 字段允许您自定义重定向策略。如果设置为 nil，则客户端将自动遵循最多 10 次重定向。
	}

	resp, err := client.Get("http://example.com")
	// 使用自定义的 client 发送 GET 请求。这与 http.Get 类似，但会应用 client 的配置。
	// ...

	req, err := http.NewRequest("GET", "http://example.com", nil)
	// 使用 http.NewRequest 创建一个更灵活的请求对象。第一个参数是 HTTP 方法（如 "GET", "POST"），
	// 第二个是 URL，第三个是请求体（如果不需要请求体，则为 nil）。
	// ...
	req.Header.Add("If-None-Match", `W/"wyzzy"`)
	// 通过 req.Header 修改请求头。Header 是一个 map[string][]string 类型，
	// Add 方法用于添加或追加一个请求头字段。
	resp, err := client.Do(req)
	// 使用自定义的 client 调用 Do 方法来执行这个构造好的请求。
	// Do 方法是 Client 的核心，它负责发送请求并返回响应。
	// ...

For control over proxies, TLS configuration, keep-alives,
compression, and other settings, create a [Transport]:
// 为了更细粒度地控制 HTTP 客户端的底层行为，例如代理设置、TLS 配置、连接复用（keep-alives）、数据压缩以及其他网络相关的参数，可以创建一个 [Transport] 实例：

	tr := &http.Transport{
		MaxIdleConns:       10, // MaxIdleConns 字段设置客户端在连接池中保持的最大空闲连接数。这些连接在完成请求后不会立即关闭，而是保持打开状态以便后续请求复用，从而减少连接建立的开销。
		IdleConnTimeout:    30 * time.Second, // IdleConnTimeout 字段定义了空闲连接在连接池中保持打开状态的最长时间。超过此时间后，空闲连接将被关闭。这有助于释放不活跃的资源。
		DisableCompression: true, // DisableCompression 字段如果设置为 true，则客户端不会自动处理响应中的 gzip 或 deflate 压缩。这意味着客户端将接收原始的、未压缩的响应体。默认情况下，客户端会自动解压缩。
	}
	client := &http.Client{Transport: tr} // 将自定义的 Transport 实例赋值给 Client 的 Transport 字段。这样，该 Client 发送的所有请求都将使用这个 Transport 定义的底层网络行为。
	resp, err := client.Get("https://example.com") // 使用配置了自定义 Transport 的 client 发送一个 GET 请求。

Clients and Transports are safe for concurrent use by multiple
goroutines and for efficiency should only be created once and re-used.
// [Client] 和 [Transport] 实例都是并发安全的，这意味着它们可以被多个 goroutine 同时安全地使用，而无需额外的同步措施。
// 为了提高效率和性能，强烈建议只创建一次这些实例，并在整个应用程序生命周期中重复使用它们，而不是为每个请求都创建新的实例。这有助于连接复用和资源管理。

# Servers
// 服务器：
// 这一部分将介绍如何使用 Go 的 net/http 包来创建和运行 HTTP 服务器。

// ListenAndServe starts an HTTP server with a given address and handler.
// ListenAndServe 函数用于启动一个 HTTP 服务器，它需要两个参数：服务器监听的地址（例如 ":8080"）和一个处理所有传入请求的处理器 (handler)。
// The handler is usually nil, which means to use [DefaultServeMux].
// 通常情况下，handler 参数会设置为 nil。当 handler 为 nil 时，http 包会自动使用全局的默认多路复用器 [DefaultServeMux] 来处理请求。
// [DefaultServeMux] 是一个内置的请求路由器，它根据请求的 URL 路径将请求分发给注册的处理器。
// [Handle] and [HandleFunc] add handlers to [DefaultServeMux]:
// [Handle] 和 [HandleFunc] 这两个函数是用于向 [DefaultServeMux] 注册请求处理器的方法。
// [Handle] 接受一个路径和一个实现了 [http.Handler] 接口的类型实例。
// [HandleFunc] 是 [Handle] 的一个便捷版本，它接受一个路径和一个普通的函数（该函数签名必须是 `func(http.ResponseWriter, *http.Request)`）。

	http.Handle("/foo", fooHandler)
	// http.Handle("/foo", fooHandler) 将一个实现了 http.Handler 接口的 fooHandler 实例注册到 DefaultServeMux，
	// 当有请求访问 "/foo" 路径时，该请求将由 fooHandler 处理。

	http.HandleFunc("/bar", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello, %q", html.EscapeString(r.URL.Path))
	})
	// http.HandleFunc("/bar", ...) 将一个匿名函数注册到 DefaultServeMux，作为 "/bar" 路径的处理器。
	// 这个匿名函数接收两个参数：
	// w (http.ResponseWriter): 用于向客户端发送 HTTP 响应。通过它写入的数据将作为响应体发送。
	// r (*http.Request): 包含了客户端的 HTTP 请求信息，如请求方法、URL、请求头、请求体等。
	// fmt.Fprintf(w, ...) 用于将格式化的字符串写入响应体。
	// html.EscapeString(r.URL.Path) 对请求 URL 的路径部分进行 HTML 转义，以防止跨站脚本 (XSS) 攻击，确保输出到 HTML 页面是安全的。

	log.Fatal(http.ListenAndServe(":8080", nil))
	// log.Fatal(http.ListenAndServe(":8080", nil)) 启动 HTTP 服务器，监听在所有网络接口的 8080 端口。
	// nil 参数表示使用默认的 DefaultServeMux 来处理所有传入的 HTTP 请求。
	// ListenAndServe 是一个阻塞调用，它会一直运行直到服务器关闭或发生错误。
	// 如果 ListenAndServe 返回错误（例如端口被占用），log.Fatal 会打印错误信息并终止程序。

More control over the server's behavior is available by creating a
custom Server:

	s := &http.Server{
		Addr:           ":8080",
		Handler:        myHandler,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	log.Fatal(s.ListenAndServe())

# HTTP/2
// HTTP/2 协议支持

// Starting with Go 1.6, the http package has transparent support for the
// HTTP/2 protocol when using HTTPS.
// 从 Go 1.6 版本开始，当使用 HTTPS 时，net/http 包对 HTTP/2 协议提供了透明的支持。
// 这意味着在大多数情况下，无需额外的配置，Go 的 HTTP 客户端和服务器会自动协商并使用 HTTP/2 协议，从而利用其多路复用、头部压缩等优势。

// Programs that must disable HTTP/2 can do so by setting [Transport.TLSNextProto] (for clients) or
// [Server.TLSNextProto] (for servers) to a non-nil, empty map.
// 如果程序需要禁用 HTTP/2，可以通过以下方式实现：
// 对于客户端，可以将 [Transport] 结构体中的 `TLSNextProto` 字段设置为一个非 nil 的空 map。
// 对于服务器，可以将 [Server] 结构体中的 `TLSNextProto` 字段设置为一个非 nil 的空 map。
// `TLSNextProto` 是一个 map[string]func(string, *tls.Conn) http.HandlerSpec，它定义了 TLS 应用层协议协商 (ALPN) 的协议映射。
// 当将其设置为空 map 时，表示不接受任何应用层协议，从而阻止了 HTTP/2 的协商。

// Alternatively, the following GODEBUG settings are
// currently supported:
// 此外，还可以通过设置以下 GODEBUG 环境变量来控制 HTTP/2 的行为：

// 	GODEBUG=http2client=0  # disable HTTP/2 client support
// 	GODEBUG=http2client=0：禁用 HTTP/2 客户端支持。当此环境变量设置为 0 时，即使服务器支持 HTTP/2，客户端也会强制使用 HTTP/1.1。

// 	GODEBUG=http2server=0  # disable HTTP/2 server support
// 	GODEBUG=http2server=0：禁用 HTTP/2 服务器支持。当此环境变量设置为 0 时，服务器将不会协商或使用 HTTP/2 协议，只提供 HTTP/1.1 服务。

// 	GODEBUG=http2debug=1   # enable verbose HTTP/2 debug logs
// 	GODEBUG=http2debug=1：启用详细的 HTTP/2 调试日志。这会在标准错误输出中打印 HTTP/2 协议的内部操作和事件，有助于调试问题。

// 	GODEBUG=http2debug=2   # ... even more verbose, with frame dumps
// 	GODEBUG=http2debug=2：启用更详细的 HTTP/2 调试日志，包括 HTTP/2 帧的原始数据转储。这对于深入分析协议行为非常有用。

// Please report any issues before disabling HTTP/2 support: https://golang.org/s/http2bug
// 在禁用 HTTP/2 支持之前，请务必报告您遇到的任何问题。
// 您可以通过访问提供的链接 (https://golang.org/s/http2bug) 来提交错误报告或查看现有问题。
// 鼓励用户报告问题是为了帮助 Go 团队改进 HTTP/2 的实现，而不是简单地禁用它。
// 报告问题有助于社区共同提升 Go 的网络功能。

The http package's [Transport] and [Server] both automatically enable
HTTP/2 support for simple configurations. To enable HTTP/2 for more
complex configurations, to use lower-level HTTP/2 features, or to use
a newer version of Go's http2 package, import "golang.org/x/net/http2"
directly and use its ConfigureTransport and/or ConfigureServer
functions. Manually configuring HTTP/2 via the golang.org/x/net/http2
package takes precedence over the net/http package's built-in HTTP/2
support.
// net/http 包中的 [Transport] (用于客户端) 和 [Server] (用于服务器) 结构体在默认情况下，
// 对于简单的配置会自动启用 HTTP/2 支持。这意味着在大多数标准使用场景下，
// 无需额外配置即可享受 HTTP/2 的优势。
//
// 然而，如果需要进行更复杂的 HTTP/2 配置，例如自定义帧类型、流控制参数，
// 或者需要访问更底层的 HTTP/2 特性，又或者希望使用 Go 官方维护的、
// 可能包含最新功能或修复的 `golang.org/x/net/http2` 包的更新版本时，
// 建议直接导入 "golang.org/x/net/http2" 包。
//
// 导入该扩展包后，可以使用其提供的 `ConfigureTransport` 和/或 `ConfigureServer` 函数。
// 这些函数允许开发者对 HTTP/2 的行为进行更精细的控制和定制。
//
// 值得注意的是，通过 `golang.org/x/net/http2` 包手动配置 HTTP/2 的方式，
// 将会优先于 `net/http` 包内置的 HTTP/2 支持。这意味着如果同时使用了两种配置方式，
// 外部包的配置会覆盖标准库的默认行为，从而确保开发者对 HTTP/2 的行为拥有最终的控制权。
// 这为需要高度定制或利用最新 HTTP/2 特性的应用程序提供了灵活性。
*/
package http
