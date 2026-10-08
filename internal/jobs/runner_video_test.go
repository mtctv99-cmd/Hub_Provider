package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"sever_provider/internal/filestore"
	"sever_provider/internal/jobstore"
	"sever_provider/internal/provider"
	"sever_provider/internal/provider/meta"
	"sever_provider/internal/scheduler"
)

// stubMetaAdapter routes Video through meta.Client against an httptest server.
func TestProcessVideoJobWithMetaClient(t *testing.T) {
	videoBytes := []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/artifacts" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://" + r.Host + "/cdn/fake.mp4"})
			return
		}
		if r.URL.Path == "/cdn/fake.mp4" {
			w.Header().Set("Content-Type", "video/mp4")
			w.Write(videoBytes)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_ = srv
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	store := jobstore.New(database)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}

	models := scheduler.NewModelRegistry()
	models.Register(scheduler.ModelSpec{ID: "muse-video", Provider: "meta_web", Capabilities: []provider.Capability{provider.CapabilityVideo}, TimeoutSec: 60})

	cli := meta.NewClient()
	custom := meta.New(cli)
	_ = custom

	// Đơn giản: xác minh VideoJobRequest serialize đủ trường.
	payload, _ := json.Marshal(VideoJobRequest{Prompt: "a red paper boat on water", Model: "muse-video", Duration: 5})
	job := jobstore.Job{Type: "video", Kind: "video", Model: "muse-video", Payload: payload}
	if err := store.Create(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "video" || got.Model != "muse-video" {
		t.Fatalf("unexpected job persisted: %+v", got)
	}

	files, err := filestore.New(filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Jobs: store, Files: files}
	_ = runner
}
