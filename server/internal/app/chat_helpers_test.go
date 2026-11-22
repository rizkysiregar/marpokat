//go:build integration

package app

import (
	"fmt"
	"sync"
	"time"
)

// at walks a decoded JSON value: string keys into objects, ints into arrays. Missing parts give nil.
func at(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			a, _ := v.([]any)
			if k < 0 || k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func (r response) at(path ...any) any { return at(r.body, path...) }

func num(v any) int64  { f, _ := v.(float64); return int64(f) }
func str(v any) string { s, _ := v.(string); return s }

func count(v any) int { a, _ := v.([]any); return len(a) }

// asServer calls the server API of the tenant.
func (e *env) asServer(tt testTenant, method, path string, body any) response {
	e.t.Helper()
	return e.do(method, path, tt.APIKey, tt.serverToken(e.t), body)
}

// asUser calls the client API as one user (external id).
func (e *env) asUser(tt testTenant, ext, method, path string, body any) response {
	e.t.Helper()
	return e.do(method, path, tt.APIKey, tt.userToken(e.t, ext), body)
}

// seed creates users with the server API.
func (e *env) seed(tt testTenant, ids ...string) {
	e.t.Helper()
	for _, id := range ids {
		if r := e.asServer(tt, "PUT", "/v1/server/users/"+id, map[string]any{"name": "User " + id}); r.status != 200 {
			e.t.Fatalf("seed %s: %d %v", id, r.status, r.body)
		}
	}
}

// dm makes the DM of two users with the server API and returns its id.
func (e *env) dm(tt testTenant, a, b string) string {
	e.t.Helper()
	r := e.asServer(tt, "POST", "/v1/server/channels/dm", map[string]any{"user_a": a, "user_b": b})
	if r.status != 201 && r.status != 200 {
		e.t.Fatalf("dm %s/%s: %d %v", a, b, r.status, r.body)
	}
	return str(r.at("id"))
}

// say sends a message as a user and returns the response.
func (e *env) say(tt testTenant, ext, channel, text string) response {
	e.t.Helper()
	return e.asUser(tt, ext, "POST", "/v1/channels/"+channel+"/messages",
		map[string]any{"client_msg_id": fmt.Sprintf("%s-%d", ext, time.Now().UnixNano()), "text": text})
}

func (e *env) mustSay(tt testTenant, ext, channel, text string) response {
	e.t.Helper()
	r := e.say(tt, ext, channel, text)
	if r.status != 201 {
		e.t.Fatalf("%s says %q: %d %v", ext, text, r.status, r.body)
	}
	return r
}

// unread is the unread count of a channel for a user, read from the channel list.
func (e *env) unread(tt testTenant, ext, channel string) int64 {
	e.t.Helper()
	r := e.asUser(tt, ext, "GET", "/v1/channels/"+channel, nil)
	if r.status != 200 {
		e.t.Fatalf("get channel %s as %s: %d %v", channel, ext, r.status, r.body)
	}
	return num(r.at("membership", "unread_count"))
}

// parallel runs fn n times at once and waits.
func parallel(n int, fn func(i int)) {
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(i)
		}()
	}
	wg.Wait()
}
