package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/mmedum/google-mail-mcp/internal/gapi"
	"github.com/mmedum/google-mail-mcp/internal/gapi/gmailtest"
	"github.com/mmedum/google-mail-mcp/internal/service"
)

// meter stands between the client and the fake and measures how many
// per-row reads are in flight at once. It holds each read for less time
// the later it arrives, so reads finish in the reverse of the order they
// were started and a result kept in completion order would show it.
type meter struct {
	mu       sync.Mutex
	inFlight int
	most     int
	arrived  int
	// answer, when set, may answer a request itself instead of the fake,
	// which it is handed as next.
	answer func(w http.ResponseWriter, r *http.Request, next http.Handler) bool
}

// isRowRead is a get of one thread, message, draft or label.
func isRowRead(r *http.Request) bool {
	rest := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/")
	kind, id, ok := strings.Cut(rest, "/")
	return ok && id != "" && !strings.Contains(id, "/") &&
		slices.Contains([]string{"threads", "messages", "drafts", "labels"}, kind)
}

func (m *meter) serve(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isRowRead(r) {
			if m.answer == nil || !m.answer(w, r, next) {
				next.ServeHTTP(w, r)
			}
			return
		}
		m.mu.Lock()
		m.inFlight++
		m.most = max(m.most, m.inFlight)
		hold := time.Duration(max(0, 8-m.arrived%8)) * 5 * time.Millisecond
		m.arrived++
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			m.inFlight--
			m.mu.Unlock()
		}()
		time.Sleep(hold)
		if m.answer == nil || !m.answer(w, r, next) {
			next.ServeHTTP(w, r)
		}
	})
}

func (m *meter) max() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.most
}

// metered is a service whose requests pass through a meter.
func metered(t *testing.T) (*service.Service, *gapi.Client, *meter) {
	s, c, m, _ := meteredFake(t)
	return s, c, m
}

func meteredFake(t *testing.T) (*service.Service, *gapi.Client, *meter, *gmailtest.Server) {
	t.Helper()
	fake := gmailtest.New()
	t.Cleanup(fake.Close)
	target, err := url.Parse(fake.URL())
	if err != nil {
		t.Fatal(err)
	}
	m := &meter{}
	proxy := httptest.NewServer(m.serve(httputil.NewSingleHostReverseProxy(target)))
	t.Cleanup(proxy.Close)
	c := gapi.New(gapi.Options{
		BaseURL:     proxy.URL,
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}),
		Sleep:       func(context.Context, time.Duration) error { return nil },
	})
	return service.New(c), c, m, fake
}

// Each listing reads its rows concurrently, at most five at a time, and
// returns them in the order the listing gave them.
func TestListingsReadRowsConcurrentlyInOrder(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		got  func(*service.Service) ([]string, error)
		want func(*gapi.Client) ([]string, error)
	}{
		{"search_threads",
			func(s *service.Service) ([]string, error) {
				l, _, err := s.SearchThreads(ctx, service.Search{})
				ids := []string{}
				for _, th := range l.Threads {
					ids = append(ids, th.ID)
				}
				return ids, err
			},
			func(c *gapi.Client) ([]string, error) {
				r, err := c.ListThreads(ctx, gapi.ListOptions{Max: service.DefaultMax})
				if err != nil {
					return nil, err
				}
				ids := []string{}
				for _, th := range r.Threads {
					ids = append(ids, th.ID)
				}
				return ids, nil
			}},
		{"search_messages",
			func(s *service.Service) ([]string, error) {
				l, _, err := s.SearchMessages(ctx, service.Search{})
				ids := []string{}
				for _, m := range l.Messages {
					ids = append(ids, m.ID)
				}
				return ids, err
			},
			func(c *gapi.Client) ([]string, error) {
				r, err := c.ListMessages(ctx, gapi.ListOptions{Max: service.DefaultMax})
				if err != nil {
					return nil, err
				}
				ids := []string{}
				for _, m := range r.Messages {
					ids = append(ids, m.ID)
				}
				return ids, nil
			}},
		{"list_labels with counts",
			func(s *service.Service) ([]string, error) {
				ls, err := s.Labels(ctx, true)
				ids := []string{}
				for _, l := range ls {
					ids = append(ids, l.ID)
				}
				return ids, err
			},
			func(*gapi.Client) ([]string, error) {
				// Without counts nothing is read per row: the reference order.
				s, _, _ := metered(t)
				ls, err := s.Labels(ctx, false)
				ids := []string{}
				for _, l := range ls {
					ids = append(ids, l.ID)
				}
				return ids, err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, m := metered(t)
			want, err := tc.want(c)
			if err != nil {
				t.Fatal(err)
			}
			if len(want) < 6 {
				t.Fatalf("only %d rows: too few to tell five in flight from more", len(want))
			}
			got, err := tc.got(s)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("order = %v\nwant    %v", got, want)
			}
			if n := m.max(); n < 2 || n > 5 {
				t.Errorf("at most %d reads in flight; want between 2 and 5", n)
			}
		})
	}
}

// The fake mailbox holds one draft, so the listing here is eight
// stand-ins for it, each read as the real draft under its own id.
func TestDraftsReadRowsConcurrentlyInOrder(t *testing.T) {
	s, _, m, fake := meteredFake(t)
	realID := fake.Scenario(gmailtest.ScenarioDraftReply).DraftID
	var want []string
	for i := range 8 {
		want = append(want, fmt.Sprintf("r-90000000%d", i))
	}
	m.answer = func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if strings.HasSuffix(r.URL.Path, "/drafts") {
			rows := []map[string]string{}
			for _, id := range want {
				rows = append(rows, map[string]string{"id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"drafts": rows})
			return true
		}
		id, ok := strings.CutPrefix(r.URL.Path, "/gmail/v1/users/me/drafts/")
		if !ok {
			return false
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/gmail/v1/users/me/drafts/" + realID
		r2.URL.RawPath = ""
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r2)
		var d map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Errorf("draft: %v", err)
			return false
		}
		d["id"] = id
		_ = json.NewEncoder(w).Encode(d)
		return true
	}
	l, err := s.Drafts(context.Background(), "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range l.Drafts {
		got = append(got, d.ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v\nwant    %v", got, want)
	}
	if n := m.max(); n < 2 || n > 5 {
		t.Errorf("at most %d reads in flight; want between 2 and 5", n)
	}
}

// The error a listing returns is the first failing row's, by position,
// whichever failed first in time.
func TestTheFirstFailingRowDecidesTheError(t *testing.T) {
	ctx := context.Background()
	s, c, m := metered(t)
	r, err := c.ListMessages(ctx, gapi.ListOptions{Max: service.DefaultMax})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) < 4 {
		t.Fatalf("only %d messages", len(r.Messages))
	}
	early, late := r.Messages[1].ID, r.Messages[3].ID
	m.answer = func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+early):
			time.Sleep(80 * time.Millisecond)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"gone"}}`))
			return true
		case strings.HasSuffix(r.URL.Path, "/"+late):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"no"}}`))
			return true
		}
		return false
	}
	for range 3 {
		_, _, err := s.SearchMessages(ctx, service.Search{})
		wantClass(t, err, gapi.ClassNotFound)
	}
}
