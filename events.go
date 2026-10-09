package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

type event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Hub 向所有在线页面广播事件（扫描进度、他人审阅结论、认领变化）。
type Hub struct {
	mu   sync.Mutex
	next int
	subs map[int]chan []byte
	// onConnect 建连时调用一次，返回首帧要下发的内容（可以是 nil）。
	// 仪表盘靠它在 SSE 建好的那一瞬间就拿到全量快照，不必先 GET 一次 /api/dashboard ——
	// 这样"连上即完整"，中间不会出现一帧空白或数字对不上的空窗。
	// 审阅页不需要首帧，返回 nil 即可。
	onConnect func() any
}

func NewHub() *Hub {
	return &Hub{subs: map[int]chan []byte{}}
}

// SetOnConnect 注册建连回调（只能设一次，重复调用直接忽略 —— 它在服务启动期
// 由装配代码调用一次，不该被运行期误改）。
func (h *Hub) SetOnConnect(f func() any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.onConnect == nil {
		h.onConnect = f
	}
}

func (h *Hub) Broadcast(typ string, data any) {
	b, err := json.Marshal(event{Type: typ, Data: data})
	if err != nil {
		return
	}
	h.mu.Lock()
	subs := make([]chan []byte, 0, len(h.subs))
	for _, c := range h.subs {
		subs = append(subs, c)
	}
	h.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- b:
		default: // 订阅者消费不过来就丢弃，进度类事件不需要保证送达
		}
	}
}

func (h *Hub) subscribe() (int, chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := h.next
	ch := make(chan []byte, 32)
	h.subs[id] = ch
	return id, ch
}

func (h *Hub) unsubscribe(id int) {
	h.mu.Lock()
	delete(h.subs, id)
	h.mu.Unlock()
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	id, ch := h.subscribe()
	defer h.unsubscribe(id)

	// 首帧在注册订阅**之后**算：这样从"开始算"到"能收事件"之间发生的任何变化
	// 都不会漏掉。顺序反过来的话，那段时间里的变更既不在首帧里、也没被订阅到，
	// 页面就会一直停在旧数字上，直到下一次该区块变化才纠正过来。
	var first []byte
	if h.onConnect != nil {
		if v := h.onConnect(); v != nil {
			first, _ = json.Marshal(event{Type: "init", Data: v})
		}
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	if first != nil {
		w.Write([]byte("data: "))
		w.Write(first)
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}

	tick := make(chan struct{}, 1)
	go func() {
		// 心跳，防止中间层掐掉空闲连接
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(25 * time.Second):
				select {
				case tick <- struct{}{}:
				default:
				}
			}
		}
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			w.Write([]byte("data: "))
			w.Write(b)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		case <-tick:
			w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}
