package dashweb

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSnapshot(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadRewardFlowStates(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name      string
		path      string
		available bool
		stale     bool
		age       int64
		errPart   string
	}{
		{"fresh", writeSnapshot(t, dir, "fresh.json", `{"version":1,"generated_at":1799999940}`), true, false, 60, ""},
		{"5 min old is not stale", writeSnapshot(t, dir, "edge.json", `{"version":1,"generated_at":1799999700}`), true, false, 300, ""},
		{"older than 5 min is stale", writeSnapshot(t, dir, "old.json", `{"version":1,"generated_at":1799999699}`), true, true, 301, ""},
		{"missing file", filepath.Join(dir, "none.json"), false, false, 0, "no reward-flow snapshot"},
		{"other version", writeSnapshot(t, dir, "v2.json", `{"version":2,"generated_at":1800000000}`), false, false, 0, "version 2"},
		{"half a file", writeSnapshot(t, dir, "cut.json", `{"version":1,"generated_at":18`), false, false, 0, "not valid JSON"},
		{"off", "", false, false, 0, "is off"},
	}
	for _, c := range cases {
		got := readRewardFlow(c.path, now)
		if got.Available != c.available || got.Stale != c.stale || got.AgeSeconds != c.age {
			t.Errorf("%s: available %v stale %v age %d, want %v %v %d", c.name, got.Available, got.Stale, got.AgeSeconds, c.available, c.stale, c.age)
		}
		if !strings.Contains(got.Error, c.errPart) || (c.errPart == "") != (got.Error == "") {
			t.Errorf("%s: error %q, want it to contain %q", c.name, got.Error, c.errPart)
		}
	}
}

// The UI reads snapshot.* as an object: the snapshot must not come back as a
// string (for example base64 of []byte).
func TestRewardFlowEndpointReturnsTheSnapshotAsAnObject(t *testing.T) {
	path := writeSnapshot(t, t.TempDir(), "dashboard.json",
		fmt.Sprintf(`{"version":1,"generated_at":%d,"state":{"paused":true}}`, time.Now().Unix()-30))
	s := &Server{rewardFlow: path}
	rec := httptest.NewRecorder()
	s.handleRewardFlow(rec, httptest.NewRequest("GET", "/api/reward-flow", nil))

	var got struct {
		Available bool `json:"available"`
		Stale     bool `json:"stale"`
		Snapshot  struct {
			Version int `json:"version"`
			State   struct {
				Paused bool `json:"paused"`
			} `json:"state"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if !got.Available || got.Stale || got.Snapshot.Version != 1 || !got.Snapshot.State.Paused {
		t.Fatalf("got %+v from %s", got, rec.Body.String())
	}
}
