package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

func authoredPull(id string) attention.Item {
	item := review(id)
	item.Label = "rebase required"
	item.Context = map[string]string{"role": "author", "repo": "didx-xyz/tofu", "number": "42"}
	return item
}

func post(t *testing.T, handler http.Handler, path string, header bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, nil)
	if header {
		request.Header.Set("X-Attentiond", "1")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestWatchAndUnwatchNeedTheHeader(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{}, &now)
	store.ReplaceSource("github", []attention.Item{authoredPull("didx-xyz/tofu#42")})

	for _, decision := range []string{"watch", "unwatch"} {
		path := "/api/items/github:didx-xyz%2Ftofu%2342/" + decision
		if recorder := post(t, handler, path, false); recorder.Code != http.StatusForbidden {
			t.Errorf("%s without the header = %d, want 403: %s", decision, recorder.Code, recorder.Body)
		}
	}
	if len(store.Watches()) != 0 {
		t.Fatal("a request without the header still started a watch")
	}
}

func TestOnlyAPullRequestYouAuthoredCanBeWatched(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{}, &now)
	store.ReplaceSource("github", []attention.Item{
		authoredPull("didx-xyz/tofu#42"),
		review("didx-xyz/tofu#7"),
	})
	store.Put(attention.Item{
		ID: attention.Key("tofu", "plan"), Source: "tofu", Title: "plan",
		State: attention.StateNeedsAttention, Context: map[string]string{"role": "author"},
	})

	cases := map[string]struct {
		path string
		want int
	}{
		"somebody else's pull request": {"/api/items/github:didx-xyz%2Ftofu%237/watch", http.StatusBadRequest},
		"not a pull request":           {"/api/items/tofu:plan/watch", http.StatusBadRequest},
		"item not held":                {"/api/items/github:didx-xyz%2Ftofu%23999/watch", http.StatusNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if recorder := post(t, handler, tc.path, true); recorder.Code != tc.want {
				t.Fatalf("got %d want %d: %s", recorder.Code, tc.want, recorder.Body)
			}
		})
	}

	board := decodeList(t, do(t, handler, http.MethodGet, "/api/work", ""))
	for _, item := range board.Items {
		for _, candidate := range item.Actions {
			if candidate.ID == "watch" && item.ID != "github:didx-xyz/tofu#42" {
				t.Errorf("%s offers a watch it would refuse", item.ID)
			}
		}
	}
}

func TestWatchThroughTheHrefTheItemCarries(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{}, &now)
	store.ReplaceSource("github", []attention.Item{authoredPull("didx-xyz/tofu#42")})

	board := decodeList(t, do(t, handler, http.MethodGet, "/api/work", ""))
	watch := action(t, board.Items[0], "watch")
	if watch.Method != "POST" || !strings.HasPrefix(watch.Href, "http://127.0.0.1:7717/") {
		t.Fatalf("watch action = %+v", watch)
	}

	recorder := post(t, handler, strings.TrimPrefix(watch.Href, "http://127.0.0.1:7717"), true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("watch: %d %s", recorder.Code, recorder.Body)
	}
	var item attention.Item
	if err := json.Unmarshal(recorder.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if !item.Watched {
		t.Fatalf("the response does not say the item is watched: %s", recorder.Body)
	}

	unwatch := action(t, item, "unwatch")
	if recorder := post(t, handler, strings.TrimPrefix(unwatch.Href, "http://127.0.0.1:7717"), true); recorder.Code != http.StatusOK {
		t.Fatalf("unwatch: %d %s", recorder.Code, recorder.Body)
	}
	if len(store.Watches()) != 0 {
		t.Fatal("unwatch left the watch in place")
	}
}
