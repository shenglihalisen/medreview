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
}

func NewHub() *Hub {
	return &Hub{subs: map[int]chan []byte{}}
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

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

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
