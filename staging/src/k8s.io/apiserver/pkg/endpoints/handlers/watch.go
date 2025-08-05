/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/websocket"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream/wsstream"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
)

// nothing will ever be sent down this channel
var neverExitWatch <-chan time.Time = make(chan time.Time)

// timeoutFactory abstracts watch timeout logic for testing
type TimeoutFactory interface {
	TimeoutCh() (<-chan time.Time, func() bool)
}

// realTimeoutFactory implements timeoutFactory
type realTimeoutFactory struct {
	timeout time.Duration
}

// TimeoutCh returns a channel which will receive something when the watch times out,
// and a cleanup function to call when this happens.
func (w *realTimeoutFactory) TimeoutCh() (<-chan time.Time, func() bool) {
	if w.timeout == 0 {
		return neverExitWatch, func() bool { return false }
	}
	t := time.NewTimer(w.timeout)
	return t.C, t.Stop
}

// serveWatchHandler returns a handle to serve a watch response.
// TODO: the functionality in this method and in WatchServer.Serve is not cleanly decoupled.
// 创建并返回一个用于处理 watch 请求的 HTTP 处理器。它处理了序列化、编码和协议协商等核心逻辑。
func serveWatchHandler(watcher watch.Interface, scope *RequestScope, mediaTypeOptions negotiation.MediaTypeOptions, req *http.Request, w http.ResponseWriter, timeout time.Duration, metricsScope string) (http.Handler, error) {
	// 获取 watch 选项
	options, err := optionsForTransform(mediaTypeOptions, req)
	if err != nil {
		return nil, err
	}

	// negotiate for the stream serializer from the scope's serializer
	// 序列化器配置：根据特性门控 CBORServingAndStorage 选择编码器
	serializer, err := negotiation.NegotiateOutputMediaTypeStream(req, scope.Serializer, scope)
	if err != nil {
		return nil, err
	}
	// 获取帧写入器
	framer := serializer.StreamSerializer.Framer
	var encoder runtime.Encoder
	if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
		encoder = scope.Serializer.EncoderForVersion(runtime.UseNondeterministicEncoding(serializer.StreamSerializer.Serializer), scope.Kind.GroupVersion())
	} else {
		encoder = scope.Serializer.EncoderForVersion(serializer.StreamSerializer.Serializer, scope.Kind.GroupVersion())
	}
	// 是否使用文本帧
	useTextFraming := serializer.EncodesAsText
	if framer == nil {
		return nil, fmt.Errorf("no framer defined for %q available for embedded encoding", serializer.MediaType)
	}
	// TODO: next step, get back mediaTypeOptions from negotiate and return the exact value here
	// 设置媒体类型
	mediaType := serializer.MediaType
	switch mediaType {
	case runtime.ContentTypeJSON:
		// as-is
	case runtime.ContentTypeCBOR:
		// If a client indicated it accepts application/cbor (exactly one data item) on a
		// watch request, set the conformant application/cbor-seq media type the watch
		// response. RFC 9110 allows an origin server to deviate from the indicated
		// preference rather than send a 406 (Not Acceptable) response (see
		// https://www.rfc-editor.org/rfc/rfc9110.html#section-12.1-5).
		mediaType = runtime.ContentTypeCBORSequence
	default:
		mediaType += ";stream=watch"
	}

	ctx := req.Context()

	// locate the appropriate embedded encoder based on the transform
	// 选择嵌入编码器
	var negotiatedEncoder runtime.Encoder
	// 获取目标编码器
	contentKind, contentSerializer, transform := targetEncodingForTransform(scope, mediaTypeOptions, req)
	// 如果需要转换
	if transform {
		// 获取目标编码器信息
		info, ok := runtime.SerializerInfoForMediaType(contentSerializer.SupportedMediaTypes(), serializer.MediaType)
		if !ok {
			return nil, fmt.Errorf("no encoder for %q exists in the requested target %#v", serializer.MediaType, contentSerializer)
		}
		// 获取目标编码器
		if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
			negotiatedEncoder = contentSerializer.EncoderForVersion(runtime.UseNondeterministicEncoding(info.Serializer), contentKind.GroupVersion())
		} else {
			negotiatedEncoder = contentSerializer.EncoderForVersion(info.Serializer, contentKind.GroupVersion())
		}
	} else {
		// 如果不需要转换
		if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
			negotiatedEncoder = scope.Serializer.EncoderForVersion(runtime.UseNondeterministicEncoding(serializer.Serializer), contentKind.GroupVersion())
		} else {
			negotiatedEncoder = scope.Serializer.EncoderForVersion(serializer.Serializer, contentKind.GroupVersion())
		}
	}

	var memoryAllocator runtime.MemoryAllocator

	// 如果支持内存分配
	if encoderWithAllocator, supportsAllocator := negotiatedEncoder.(runtime.EncoderWithAllocator); supportsAllocator {
		// don't put the allocator inside the embeddedEncodeFn as that would allocate memory on every call.
		// instead, we allocate the buffer for the entire watch session and release it when we close the connection.
		memoryAllocator = runtime.AllocatorPool.Get().(*runtime.Allocator)
		negotiatedEncoder = runtime.NewEncoderWithAllocator(encoderWithAllocator, memoryAllocator)
	}
	// 如果需要转换
	var tableOptions *metav1.TableOptions
	if options != nil {
		if passedOptions, ok := options.(*metav1.TableOptions); ok {
			tableOptions = passedOptions
		} else {
			return nil, fmt.Errorf("unexpected options type: %T", options)
		}
	}
	// 创建嵌入编码器
	embeddedEncoder := newWatchEmbeddedEncoder(ctx, negotiatedEncoder, mediaTypeOptions.Convert, tableOptions, scope)

	// 如果支持内存分配
	if encoderWithAllocator, supportsAllocator := encoder.(runtime.EncoderWithAllocator); supportsAllocator {
		// 如果支持内存分配
		if memoryAllocator == nil {
			// don't put the allocator inside the embeddedEncodeFn as that would allocate memory on every call.
			// instead, we allocate the buffer for the entire watch session and release it when we close the connection.
			memoryAllocator = runtime.AllocatorPool.Get().(*runtime.Allocator)
		}
		// 创建内存分配器
		encoder = runtime.NewEncoderWithAllocator(encoderWithAllocator, memoryAllocator)
	}
	// 如果需要转换
	var serverShuttingDownCh <-chan struct{}
	if signals := apirequest.ServerShutdownSignalFrom(req.Context()); signals != nil {
		serverShuttingDownCh = signals.ShuttingDown()
	}

	// 创建 watch 服务器
	server := &WatchServer{
		Watching: watcher,
		Scope:    scope,

		UseTextFraming:  useTextFraming,
		MediaType:       mediaType,
		Framer:          framer,
		Encoder:         encoder,
		EmbeddedEncoder: embeddedEncoder,

		MemoryAllocator:      memoryAllocator,
		TimeoutFactory:       &realTimeoutFactory{timeout},
		ServerShuttingDownCh: serverShuttingDownCh,

		metricsScope: metricsScope,
	}

	/****协议处理****/
	// 如果是 WebSocket 请求
	if wsstream.IsWebSocketRequest(req) {
		w.Header().Set("Content-Type", server.MediaType)
		return websocket.Handler(server.HandleWS), nil
	}
	// 如果是 HTTP 请求
	return http.HandlerFunc(server.HandleHTTP), nil
}

// WatchServer serves a watch.Interface over a websocket or vanilla HTTP.
// Kubernetes watch 机制的核心部分，它允许客户端实时接收资源变更的通知。
// 同时支持 WebSocket 和 HTTP 流式传输，并提供了可配置的编码和帧处理选项
type WatchServer struct {
	// 实现 watch.Interface 接口，负责实际的 watch 操作
	Watching watch.Interface
	// RequestScope: 请求特定的信息和元数据
	Scope *RequestScope

	/***** 消息格式化 *****/
	// true if websocket messages should use text framing (as opposed to binary framing)
	// WebSocket 消息使用文本帧(true)还是二进制帧(false)
	UseTextFraming bool
	// the media type this watch is being served with
	// 服务端返回的消息类型：指定 watch 流的媒体类型（如 "application/json"）
	MediaType string
	// used to frame the watch stream
	// 用于帧处理：处理 watch 流的帧封装
	Framer runtime.Framer
	// used to encode the watch stream event itself
	// 用于编码 watch 流事件
	Encoder runtime.Encoder
	// used to encode the nested object in the watch stream
	// 用于编码 watch 流中的嵌套对象
	EmbeddedEncoder runtime.Encoder

	/***** 资源管理 *****/
	// 内存分配器：管理 watch 操作的内存分配
	MemoryAllocator runtime.MemoryAllocator
	// 超时工厂：用于管理 watch 操作的超时
	TimeoutFactory TimeoutFactory
	// 服务器关闭信号通道：用于通知 watch 操作在服务器关闭时停止
	ServerShuttingDownCh <-chan struct{}

	/***** 监控 *****/
	// metricsScope: 监控指标的范围
	metricsScope string
}

// HandleHTTP serves a series of encoded events via HTTP with Transfer-Encoding: chunked.
// or over a websocket connection.
func (s *WatchServer) HandleHTTP(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if s.MemoryAllocator != nil {
			runtime.AllocatorPool.Put(s.MemoryAllocator)
		}
	}()

	// 检查是否支持 http.Flusher，HTTP 分块传输编码（Transfer-Encoding: chunked）
	flusher, ok := w.(http.Flusher)
	if !ok {
		err := fmt.Errorf("unable to start watch - can't get http.Flusher: %#v", w)
		utilruntime.HandleError(err)
		s.Scope.err(errors.NewInternalError(err), w, req)
		return
	}

	// 创建帧写入器
	framer := s.Framer.NewFrameWriter(w)
	if framer == nil {
		// programmer error
		err := fmt.Errorf("no stream framing support is available for media type %q", s.MediaType)
		utilruntime.HandleError(err)
		s.Scope.err(errors.NewBadRequest(err.Error()), w, req)
		return
	}

	// ensure the connection times out
	// 确保连接超时
	timeoutCh, cleanup := s.TimeoutFactory.TimeoutCh()
	defer cleanup()

	// begin the stream
	// 设置 HTTP 响应头
	w.Header().Set("Content-Type", s.MediaType)
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	// 立即发送数据，刷新 HTTP 响应，确保客户端能够及时接收到响应
	flusher.Flush()

	// 获取资源类型
	gvr := s.Scope.Resource
	// 创建 watch 编码器
	watchEncoder := newWatchEncoder(req.Context(), gvr, s.EmbeddedEncoder, s.Encoder, framer)
	// 获取 watch 事件通道
	ch := s.Watching.ResultChan()
	// 获取请求上下文的 Done 通道
	done := req.Context().Done()

	// 开始处理 watch 事件
	for {
		select {
		case <-s.ServerShuttingDownCh:
			// the server has signaled that it is shutting down (not accepting
			// any new request), all active watch request(s) should return
			// immediately here. The WithWatchTerminationDuringShutdown server
			// filter will ensure that the response to the client is rate
			// limited in order to avoid any thundering herd issue when the
			// client(s) try to reestablish the WATCH on the other
			// available apiserver instance(s).
			return
		case <-done:
			return
		case <-timeoutCh:
			return
		// 处理 watch 事件
		case event, ok := <-ch:
			if !ok {
				// End of results.
				return
			}
			metrics.WatchEvents.WithContext(req.Context()).WithLabelValues(gvr.Group, gvr.Version, gvr.Resource).Inc()
			isWatchListLatencyRecordingRequired := shouldRecordWatchListLatency(event)

			// 编码 watch 事件
			if err := watchEncoder.Encode(event); err != nil {
				utilruntime.HandleError(err)
				// client disconnect.
				return
			}

			if len(ch) == 0 {
				flusher.Flush()
			}
			if isWatchListLatencyRecordingRequired {
				metrics.RecordWatchListLatency(req.Context(), s.Scope.Resource, s.metricsScope)
			}
		}
	}
}

// HandleWS serves a series of encoded events over a websocket connection.
// 处理 WebSocket 连接的 watch 服务器
func (s *WatchServer) HandleWS(ws *websocket.Conn) {
	// 资源清理:确保内存分配器被正确释放
	defer func() {
		if s.MemoryAllocator != nil {
			runtime.AllocatorPool.Put(s.MemoryAllocator)
		}
	}()

	defer ws.Close()
	done := make(chan struct{})
	// ensure the connection times out
	// 超时处理
	timeoutCh, cleanup := s.TimeoutFactory.TimeoutCh()
	defer cleanup()

	// 监听客户端关闭事件
	go func() {
		defer utilruntime.HandleCrash()
		// This blocks until the connection is closed.
		// Client should not send anything.
		wsstream.IgnoreReceives(ws, 0)
		// Once the client closes, we should also close
		close(done)
	}()

	// 创建帧写入器
	framer := newWebsocketFramer(ws, s.UseTextFraming)

	gvr := s.Scope.Resource
	// 创建 watch 编码器
	watchEncoder := newWatchEncoder(context.TODO(), gvr, s.EmbeddedEncoder, s.Encoder, framer)
	// 获取 watch 事件通道
	ch := s.Watching.ResultChan()

	// 处理 watch 事件
	for {
		select {
		case <-done:
			return
		case <-timeoutCh:
			return
		case event, ok := <-ch:
			if !ok {
				// End of results.
				return
			}

			if err := watchEncoder.Encode(event); err != nil {
				utilruntime.HandleError(err)
				// client disconnect.
				return
			}
		}
	}
}

type websocketFramer struct {
	ws             *websocket.Conn
	useTextFraming bool
}

func newWebsocketFramer(ws *websocket.Conn, useTextFraming bool) io.Writer {
	return &websocketFramer{
		ws:             ws,
		useTextFraming: useTextFraming,
	}
}

func (w *websocketFramer) Write(p []byte) (int, error) {
	if w.useTextFraming {
		// bytes.Buffer::String() has a special handling of nil value, but given
		// we're writing serialized watch events, this will never happen here.
		if err := websocket.Message.Send(w.ws, string(p)); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if err := websocket.Message.Send(w.ws, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

var _ io.Writer = &websocketFramer{}

func shouldRecordWatchListLatency(event watch.Event) bool {
	if event.Type != watch.Bookmark || !utilfeature.DefaultFeatureGate.Enabled(features.WatchList) {
		return false
	}
	// as of today the initial-events-end annotation is added only to a single event
	// by the watch cache and only when certain conditions are met
	//
	// for more please read https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/3157-watch-list
	hasAnnotation, err := storage.HasInitialEventsEndBookmarkAnnotation(event.Object)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to determine if the obj has the required annotation for measuring watchlist latency, obj %T: %v", event.Object, err))
		return false
	}
	return hasAnnotation
}
