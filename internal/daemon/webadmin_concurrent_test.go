package daemon

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/rceman/reposuite-relay/internal/adminauth"
)

// TestConcurrentSetupCommitsOnce: serialized commit + atomic create-once
// mean exactly one of N racing setup requests wins; the rest see 409.
func TestConcurrentSetupCommitsOnce(t *testing.T) {
	d, _ := webDaemon(t)
	setupToken := d.web.mgr.FormToken("setup")
	const n = 8
	results := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := webForm(d, "setup", url.Values{"form_token": {setupToken},
				"username": {fmt.Sprintf("admin%d", i)},
				"password": {"correct-horse-12"}, "confirm": {"correct-horse-12"}})
			resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}}).Do(req)
			if err != nil {
				results <- -1
				return
			}
			resp.Body.Close()
			results <- resp.StatusCode
		}(i)
	}
	wg.Wait()
	close(results)
	wins := 0
	for code := range results {
		if code == http.StatusSeeOther {
			wins++
		} else if code != http.StatusConflict {
			t.Fatalf("unexpected setup result %d", code)
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent setups committed, want exactly 1", wins)
	}
	if _, err := adminauth.Load(d.paths.AdminCredentials()); err != nil {
		t.Fatalf("credential not committed: %v", err)
	}
}
