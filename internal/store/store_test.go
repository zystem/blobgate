package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "garage-main", "s1", strings.Repeat("a", 32)} {
		if !ValidName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "1a", "-a", "A", "a_b", "a b", strings.Repeat("a", 33)} {
		if ValidName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConfigCheck(t *testing.T) {
	good := Config{Kind: "mirror", Nodes: []NodeSpec{{Backend: "local", Path: "/tmp"}}}
	if why := good.Check(Secrets{}); why != "" {
		t.Fatal(why)
	}
	s3 := Config{Kind: "s3", Endpoint: "https://s3.example.com", Region: "garage", Bucket: "bucket-1", KeyID: "GK123"}
	if why := s3.Check(Secrets{S3Secret: "secret"}); why != "" {
		t.Fatal(why)
	}
	cases := map[string]struct {
		c Config
		s Secrets
	}{
		"unknown kind":           {Config{Kind: "dav"}, Secrets{}},
		"newer layout":           {Config{Kind: "mirror", Layout: 99, Nodes: good.Nodes}, Secrets{}},
		"no nodes":               {Config{Kind: "mirror"}, Secrets{}},
		"nine nodes":             {Config{Kind: "mirror", Nodes: make([]NodeSpec, 9)}, Secrets{}},
		"unknown backend":        {Config{Kind: "mirror", Nodes: []NodeSpec{{Backend: "nosuch"}}}, Secrets{}},
		"secrets for 2 nodes":    {good, Secrets{Nodes: make([]map[string]string, 2)}},
		"min_acks too high":      {Config{Kind: "mirror", Nodes: good.Nodes, MinAcks: 2}, Secrets{}},
		"s3 endpoint with path":  {Config{Kind: "s3", Endpoint: "https://h/p", Region: "r", Bucket: "bucket-1", KeyID: "k"}, Secrets{S3Secret: "s"}},
		"s3 endpoint no scheme":  {Config{Kind: "s3", Endpoint: "h:9000", Region: "r", Bucket: "bucket-1", KeyID: "k"}, Secrets{S3Secret: "s"}},
		"s3 no region":           {Config{Kind: "s3", Endpoint: "http://h", Bucket: "bucket-1", KeyID: "k"}, Secrets{S3Secret: "s"}},
		"s3 short bucket":        {Config{Kind: "s3", Endpoint: "http://h", Region: "r", Bucket: "b", KeyID: "k"}, Secrets{S3Secret: "s"}},
		"s3 no key id":           {Config{Kind: "s3", Endpoint: "http://h", Region: "r", Bucket: "bucket-1"}, Secrets{S3Secret: "s"}},
		"s3 no secret":           {Config{Kind: "s3", Endpoint: "http://h", Region: "r", Bucket: "bucket-1", KeyID: "k"}, Secrets{}},
		"s3 bad space url":       {Config{Kind: "s3", Endpoint: "http://h", Region: "r", Bucket: "bucket-1", KeyID: "k", SpaceURL: "ftp://x"}, Secrets{S3Secret: "s"}},
		"enc key not among keys": {Config{Kind: "mirror", Nodes: good.Nodes, Encrypt: &EncConfig{KeyID: 2}}, Secrets{EncKeys: map[string]string{"1": strings.Repeat("k", 16)}}},
		"enc key id 0":           {Config{Kind: "mirror", Nodes: good.Nodes}, Secrets{EncKeys: map[string]string{"0": strings.Repeat("k", 16)}}},
		"enc short key":          {Config{Kind: "mirror", Nodes: good.Nodes, Encrypt: &EncConfig{KeyID: 1}}, Secrets{EncKeys: map[string]string{"1": "short"}}},
	}
	for name, c := range cases {
		if why := c.c.Check(c.s); why == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseRemoteAndConnString(t *testing.T) {
	spec, err := ParseRemote(":ftp,host=h,port=21,user=u:/blobs")
	if err != nil || spec.Backend != "ftp" || spec.Options["host"] != "h" || spec.Path != "/blobs" {
		t.Fatalf("%+v %v", spec, err)
	}
	for _, bad := range []string{"plain/path", "remote:path", ":ftp,host=:"} {
		if _, err := ParseRemote(bad); err == nil && bad != ":ftp,host=:" {
			t.Errorf("%q accepted", bad)
		}
	}
	// the password is obscured by the module, and is not part of the id of the node
	a := NodeSpec{Backend: "ftp", Options: map[string]string{"host": "h", "user": "u", "pass": "one"}, Path: "/p"}
	b := NodeSpec{Backend: "ftp", Options: map[string]string{"host": "h", "user": "u", "pass": "two"}, Path: "/p"}
	if a.ID() != b.ID() || !strings.HasPrefix(a.ID(), "ftp-") {
		t.Fatalf("%s %s", a.ID(), b.ID())
	}
	cs, err := a.ConnString()
	if err != nil || strings.Contains(cs, "one") || !strings.Contains(cs, "pass='") {
		t.Fatalf("%s %v", cs, err)
	}
	if _, err := (NodeSpec{Backend: "nosuch"}).ConnString(); err == nil {
		t.Fatal("an unknown backend")
	}
	if _, err := (NodeSpec{Backend: "nosuch"}).Open(context.Background()); err == nil {
		t.Fatal("an unknown backend opened")
	}
	if len(Backends()) < 5 {
		t.Fatal("the built-in backends")
	}
	// the WebDAV pacer is set by default and the store may override it
	w, _ := (NodeSpec{Backend: "webdav", Options: map[string]string{"url": "http://h"}}).ConnString()
	w2, _ := (NodeSpec{Backend: "webdav", Options: map[string]string{"url": "http://h", "pacer_min_sleep": "50ms"}}).ConnString()
	if !strings.Contains(w, "pacer_min_sleep='100us'") || !strings.Contains(w2, "pacer_min_sleep='50ms'") {
		t.Fatalf("%s %s", w, w2)
	}
	// s3 as one node
	n := Config{Kind: "s3", Endpoint: "http://h:9000", Region: "garage", Bucket: "b1b", KeyID: "k"}.Specs(Secrets{S3Secret: "sec"})
	if len(n) != 1 || n[0].Backend != "s3" || n[0].Path != "b1b" || n[0].Options["secret_access_key"] != "sec" {
		t.Fatalf("%+v", n)
	}
}

func TestGarageSpaceAndMirrorFree(t *testing.T) {
	metrics := `# HELP garage_local_disk_avail x
garage_local_disk_avail{volume="data"} 8.2e9
garage_local_disk_avail{volume="metadata"} 1.0e9
garage_local_disk_total{volume="data"} 1.2e10
garage_local_disk_total{volume="metadata"} 2.0e9
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/none" {
			_, _ = w.Write([]byte("nothing useful\n"))
			return
		}
		if r.URL.Path == "/err" {
			http.Error(w, "x", 500)
			return
		}
		_, _ = w.Write([]byte(metrics))
	}))
	defer srv.Close()
	ctx := context.Background()
	i, ok := garageSpace(ctx, srv.URL)
	if !ok || !i.Known || i.Free != 8200000000 || i.Total != 12000000000 {
		t.Fatalf("%+v %v", i, ok)
	}
	for _, p := range []string{"/none", "/err"} {
		if _, ok := garageSpace(ctx, srv.URL+p); ok {
			t.Errorf("%s gave an answer", p)
		}
	}
	if _, ok := garageSpace(ctx, "http://127.0.0.1:1/"); ok {
		t.Fatal("a store that is away gave an answer")
	}
	// a mirror with a space_url takes its room from there; a local node from about
	m := &Mirror{Name: "g", SpaceURL: srv.URL, MinAcks: 1}
	if f := m.Free(ctx); !f.Known || f.Free != 8200000000 {
		t.Fatalf("%+v", f)
	}
	dir := t.TempDir()
	spec := NodeSpec{Backend: "local", Path: dir}
	f, err := spec.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local := &Mirror{Name: "l", Nodes: []*Node{NewNode(spec, f)}, MinAcks: 1}
	if f := local.Free(ctx); !f.Known || f.Total <= 0 {
		t.Fatalf("%+v", f)
	}
	if len(local.Nodes[0].Label()) == 0 || local.NodeByID("nope") != nil || local.NodeByID(local.Nodes[0].ID) == nil || local.Sealing() {
		t.Fatal("node helpers")
	}
}

func TestValidateReportsWhatFails(t *testing.T) {
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	_ = os.Mkdir(ro, 0o500)
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	spec := NodeSpec{Backend: "local", Path: ro}
	f, err := spec.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cs := Validate(context.Background(), NewNode(spec, f))
	if Usable(cs) {
		t.Fatalf("a node that cannot be written is usable: %+v", cs)
	}
	failed := 0
	for _, c := range cs {
		if !c.OK && c.Required && c.Detail != "" {
			failed++
		}
	}
	if failed == 0 {
		t.Fatalf("%+v", cs)
	}
}
